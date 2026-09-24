package githubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, token string, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(token).WithBaseURL(srv.URL + "/")
}

func TestGetRun(t *testing.T) {
	c := newTestClient(t, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runs/1842" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":1842,"name":"CI","display_title":"fix: stuff","run_number":77,"conclusion":"failure","html_url":"https://github.com/o/r/actions/runs/1842"}`)
	}))
	run, err := c.GetRun(context.Background(), "o", "r", 1842)
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != 1842 || run.Conclusion != "failure" || run.DisplayTitle != "fix: stuff" {
		t.Errorf("unexpected run: %+v", run)
	}
}

func TestJobLogsFollows302(t *testing.T) {
	sawAuthOnBlob := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/actions/jobs/42/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("auth header required on API log request")
		}
		http.Redirect(w, r, "/blob/logs/42", http.StatusFound)
	})
	mux.HandleFunc("/blob/logs/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuthOnBlob = true
		}
		fmt.Fprint(w, "2026-09-24T10:00:00.000Z error: boom")
	})
	c := newTestClient(t, "t0k3n", mux)

	got, err := c.JobLogs(context.Background(), "o", "r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if sawAuthOnBlob {
		t.Error("token must not be sent to the blob redirect target")
	}
	if !strings.Contains(got, "error: boom") {
		t.Errorf("log content = %q", got)
	}
}

func TestJobLogsExpiredGone(t *testing.T) {
	for _, code := range []int{http.StatusGone, http.StatusNotFound} {
		c := newTestClient(t, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		_, err := c.JobLogs(context.Background(), "o", "r", 42)
		if err != ErrLogsExpired {
			t.Errorf("HTTP %d: err = %v, want ErrLogsExpired", code, err)
		}
	}
}

func TestListJobsPaginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/actions/runs/7/jobs", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case 1:
			w.Header().Set("Link", `</repos/o/r/actions/runs/7/jobs?per_page=100&page=2>; rel="next"`)
			fmt.Fprint(w, `{"total_count":3,"jobs":[`+
				`{"id":1,"name":"linux / build","conclusion":"failure"},`+
				`{"id":2,"name":"linux / test","conclusion":"failure"}]}`)
		case 2:
			fmt.Fprint(w, `{"total_count":3,"jobs":[{"id":3,"name":"windows / build","conclusion":"success"}]}`)
		default:
			t.Errorf("unexpected page %d", page)
		}
	})
	c := newTestClient(t, "", mux)

	jobs, err := c.ListJobs(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || jobs[2].Name != "windows / build" {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestRateLimitRetry(t *testing.T) {
	calls := 0
	c := newTestClient(t, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"id":5,"conclusion":"failure"}`)
	}))

	run, err := c.GetRun(context.Background(), "o", "r", 5)
	if err != nil {
		t.Fatalf("rate-limit retry failed: %v", err)
	}
	if run.ID != 5 {
		t.Errorf("run = %+v", run)
	}
}

func TestRateLimitGivesUp(t *testing.T) {
	c := newTestClient(t, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	_, err := c.GetRun(context.Background(), "o", "r", 5)
	var rl *RateLimitError
	if err == nil || !errorsAs(err, &rl) {
		t.Fatalf("err = %v, want RateLimitError", err)
	}
}

func TestGetJob(t *testing.T) {
	c := newTestClient(t, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":99,"name":"build","conclusion":"failure"}`)
	}))
	job, err := c.GetJob(context.Background(), "o", "r", 99)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != 99 || job.Conclusion != "failure" {
		t.Errorf("job = %+v", job)
	}
}

func TestJobLogsAuthRequired(t *testing.T) {
	// GitHub answers 403 "Must have admin rights" for anonymous log
	// downloads, even on public repos.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/actions/jobs/42/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Must have admin rights to Repository."}`)
	})

	c := newTestClient(t, "", mux)
	_, err := c.JobLogs(context.Background(), "o", "r", 42)
	var ar *AuthRequiredError
	if !errorsAs(err, &ar) {
		t.Fatalf("err = %v, want AuthRequiredError", err)
	}

	c2 := newTestClient(t, "tok", mux)
	_, err = c2.JobLogs(context.Background(), "o", "r", 42)
	if err == nil || errorsAs(err, &ar) {
		t.Errorf("with token: err = %v, want plain HTTPError", err)
	}
}

// errorsAs is a thin wrapper over errors.As for assertions.
func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}
