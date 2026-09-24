// Package githubapi is a minimal GitHub REST v3 client for the Actions
// endpoints ci-brief needs: workflow runs, run jobs and per-job logs.
//
// Job log requests answer with 302 to a signed Azure blob URL; the client
// follows Location manually and strips credentials for the redirect target.
// Unauthenticated access works for public repos (60 req/h rate limit),
// a fine-grained PAT is recommended for series.
package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com/"
	apiVersion     = "2022-11-28"
	userAgent      = "ci-brief/0.1.0"
	maxBodyBytes   = 64 << 20 // per-job log cap
)

// ErrLogsExpired marks runs older than ~90 days whose log blobs are gone
// (HTTP 410/404 on the log endpoint); callers degrade gracefully.
var ErrLogsExpired = errors.New("job logs expired (runs older than ~90 days lose their logs)")

// AuthRequiredError is returned when GitHub answers 403 "Must have admin
// rights" — its way of saying job-log downloads need a token even for
// public repos (listing runs and jobs still works unauthenticated).
type AuthRequiredError struct {
	URL string
}

func (e *AuthRequiredError) Error() string {
	return "github requires a token to download job logs, even for public repos; set GITHUB_TOKEN or pass --token"
}

// authRequired checks a 403 body for GitHub's anonymous-access message.
func authRequired(token, body string) bool {
	return token == "" && strings.Contains(body, "Must have admin rights to Repository")
}

// HTTPError is a non-retryable API error.
type HTTPError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *HTTPError) Error() string {
	msg := strings.TrimSpace(e.Body)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if msg == "" {
		return fmt.Sprintf("github api: HTTP %d for %s", e.StatusCode, e.URL)
	}
	return fmt.Sprintf("github api: HTTP %d for %s: %s", e.StatusCode, e.URL, msg)
}

// RateLimitError means the quota is exhausted; RetryAfter reports how long
// the client already waited (it never waits longer than maxRateLimitWait).
type RateLimitError struct {
	ResetAt time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github rate limit exhausted, resets at %s (pass --token to raise it)",
		e.ResetAt.UTC().Format("15:04:05 UTC"))
}

// Run is a workflow run, only the fields ci-brief uses.
type Run struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	RunNumber    int64     `json:"run_number"`
	Conclusion   string    `json:"conclusion"`
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
}

// Job is one workflow run job.
type Job struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	StartedAt  time.Time `json:"started_at"`
}

// Client queries the GitHub API.
type Client struct {
	base     *url.URL
	http     *http.Client
	token    string
	Progress func(format string, args ...any) // optional stderr progress sink
}

// New creates a client; token may be empty for public repos.
func New(token string) *Client {
	u, _ := url.Parse(defaultBaseURL)
	return &Client{
		base: u,
		http: &http.Client{
			Timeout: 5 * time.Minute,
			// Redirects are handled manually to rewrite auth headers.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		token: token,
	}
}

// WithBaseURL points the client at another API root (tests, GHES).
func (c *Client) WithBaseURL(raw string) *Client {
	if u, err := url.Parse(raw); err == nil {
		c.base = u
	}
	return c
}

func (c *Client) progress(format string, args ...any) {
	if c.Progress != nil {
		c.Progress(format, args...)
	}
}

// do performs the request with retry on rate-limit and transient errors.
func (c *Client) do(ctx context.Context, method, path string, query url.Values) (*http.Response, error) {
	u := c.base.JoinPath(path)
	if query != nil {
		u.RawQuery = query.Encode()
	}

	const maxAttempts = 4
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("User-Agent", userAgent)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
		} else if resp.StatusCode == http.StatusOK {
			return resp, nil
		} else {
			lastErr = &HTTPError{StatusCode: resp.StatusCode, URL: u.String()}
			switch {
			case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				reset := rateReset(resp.Header, u.String())
				wait := time.Until(reset) + time.Second
				if wait > 30*time.Second || attempt == maxAttempts {
					return nil, &RateLimitError{ResetAt: reset}
				}
				c.progress("rate limited, waiting %s", wait.Round(time.Second))
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
				continue
			case resp.StatusCode == http.StatusTooManyRequests:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				reset := rateReset(resp.Header, u.String())
				wait := time.Until(reset) + time.Second
				if wait > 30*time.Second || attempt == maxAttempts {
					return nil, &RateLimitError{ResetAt: reset}
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
				continue
			case resp.StatusCode >= 500:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				// transient: fall through to backoff below
			default:
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				if resp.StatusCode == http.StatusForbidden && authRequired(c.token, string(body)) {
					return nil, &AuthRequiredError{URL: u.String()}
				}
				return nil, &HTTPError{StatusCode: resp.StatusCode, URL: u.String(), Body: string(body)}
			}
		}

		if attempt < maxAttempts {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			c.progress("request failed (%v), retrying in %s", lastErr, backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return nil, lastErr
}

func rateReset(h http.Header, rawURL string) time.Time {
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(sec, 0)
		}
	}
	return time.Now().Add(time.Minute)
}

// GetRun fetches one workflow run.
func (c *Client) GetRun(ctx context.Context, owner, repo string, runID int64) (*Run, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/%s/actions/runs/%d", owner, repo, runID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var run Run
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&run); err != nil {
		return nil, fmt.Errorf("decode run %d: %w", runID, err)
	}
	return &run, nil
}

// LatestFailedRun returns the most recent completed run with conclusion
// "failure".
func (c *Client) LatestFailedRun(ctx context.Context, owner, repo string) (*Run, error) {
	q := url.Values{"per_page": {"1"}, "status": {"completed"}, "conclusion": {"failure"}}
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/%s/actions/runs", owner, repo), q)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var envelope struct {
		TotalCount int   `json:"total_count"`
		Runs       []Run `json:"workflow_runs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode runs list: %w", err)
	}
	if len(envelope.Runs) == 0 {
		return nil, fmt.Errorf("no failed runs found for %s/%s", owner, repo)
	}
	run := envelope.Runs[0]
	return &run, nil
}

type jobsEnvelope struct {
	TotalCount int   `json:"total_count"`
	Jobs       []Job `json:"jobs"`
}

// ListJobs lists all jobs of a run, following the Link-header pagination.
func (c *Client) ListJobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	var all []Job
	q := url.Values{"per_page": {"100"}}
	for {
		resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs", owner, repo, runID), q)
		if err != nil {
			return nil, err
		}
		var env jobsEnvelope
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode jobs page: %w", err)
		}
		all = append(all, env.Jobs...)
		next := nextPage(link)
		if next == 0 {
			break
		}
		q.Set("page", strconv.Itoa(next))
	}
	return all, nil
}

// nextPage extracts the page number from the rel="next" Link header value;
// returns 0 when there is no next page.
func nextPage(link string) int {
	for _, part := range strings.Split(link, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 {
			continue
		}
		target := strings.TrimSpace(seg[0])
		target = strings.TrimPrefix(strings.TrimSuffix(target, ">"), "<")
		for _, p := range seg[1:] {
			if strings.Contains(p, `rel="next"`) || strings.Contains(p, "rel=next") {
				if u, err := url.Parse(target); err == nil {
					if n, err := strconv.Atoi(u.Query().Get("page")); err == nil {
						return n
					}
				}
			}
		}
	}
	return 0
}

// GetJob fetches a single job. The endpoint is repo-scoped: resolve owner/
// repo from --repo or the git remote before calling.
func (c *Client) GetJob(ctx context.Context, owner, repo string, jobID int64) (*Job, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/%s/actions/jobs/%d", owner, repo, jobID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var job Job
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&job); err != nil {
		return nil, fmt.Errorf("decode job %d: %w", jobID, err)
	}
	return &job, nil
}

// JobLogs downloads one job's log, following the 302 to the blob storage.
// Returns ErrLogsExpired for 404/410 (logs older than ~90 days).
func (c *Client) JobLogs(ctx context.Context, owner, repo string, jobID int64) (string, error) {
	u := c.base.JoinPath(fmt.Sprintf("repos/%s/%s/actions/jobs/%d/logs", owner, repo, jobID))

	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", userAgent)
		// Send the token only on the first API request, never to a
		// redirect (blob storage) target.
		if c.token != "" && attempt == 1 {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			switch {
			case resp.StatusCode == http.StatusOK:
				body, err := readCapped(resp)
				resp.Body.Close()
				return body, err
			case resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusTemporaryRedirect:
				loc := resp.Header.Get("Location")
				resp.Body.Close()
				if loc == "" {
					return "", fmt.Errorf("log redirect without Location for job %d", jobID)
				}
				blob, err := url.Parse(loc)
				if err != nil {
					return "", fmt.Errorf("bad log redirect for job %d: %w", jobID, err)
				}
				u = c.base.ResolveReference(blob) // absolute blob URLs survive ResolveReference
				continue
			case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				return "", ErrLogsExpired
			default:
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()
				if resp.StatusCode == http.StatusForbidden && authRequired(c.token, string(body)) {
					return "", &AuthRequiredError{URL: u.String()}
				}
				return "", &HTTPError{StatusCode: resp.StatusCode, URL: u.String(), Body: string(body)}
			}
		}
		if attempt < maxAttempts {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return "", lastErr
}

func readCapped(resp *http.Response) (string, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read log: %w", err)
	}
	return string(body), nil
}
