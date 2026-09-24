// Package logsource abstracts where job logs come from: the GitHub API or
// local files. Both feed the same []JobLog pipeline.
package logsource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/wrinfotel/ci-brief/internal/githubapi"
	"github.com/wrinfotel/ci-brief/internal/mask"
)

// JobLog is one job's full log content (already secret-masked).
type JobLog struct {
	JobID   int64
	JobName string
	Content string
	Size    int
}

// RunInfo describes the analyzed run (or a synthesized one for files).
type RunInfo struct {
	RunID       int64    `json:"id"`
	RunNumber   int64    `json:"number"`
	Workflow    string   `json:"workflow"`
	Title       string   `json:"title"`
	Conclusion  string   `json:"conclusion"`
	HTMLURL     string   `json:"url,omitempty"`
	TotalJobs   int      `json:"total_jobs"`
	FailedJobs  int      `json:"failed_jobs"`
	LogCount    int      `json:"logs"`
	LogBytes    int      `json:"log_bytes"`
	ExpiredLogs int      `json:"expired_logs,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

// Source produces job logs for one run or job.
type Source interface {
	FetchLogs(ctx context.Context) ([]JobLog, RunInfo, error)
}

// failedConclusions are the job outcomes worth analyzing.
var failedConclusions = map[string]bool{
	"failure":         true,
	"startup_failure": true,
	"timed_out":       true,
}

// APISource fetches logs from GitHub Actions.
type APISource struct {
	client  *githubapi.Client
	owner   string
	repo    string
	runID   int64
	jobID   int64 // nonzero => single-job mode
	allJobs bool
	workers int
}

// NewAPI analyzes a whole run; when allJobs is true it analyzes every job,
// not only the failed ones.
func NewAPI(client *githubapi.Client, owner, repo string, runID int64, allJobs bool) *APISource {
	return &APISource{client: client, owner: owner, repo: repo, runID: runID, allJobs: allJobs, workers: 6}
}

// NewAPIJob analyzes a single job (still repo-scoped).
func NewAPIJob(client *githubapi.Client, owner, repo string, jobID int64) *APISource {
	return &APISource{client: client, owner: owner, repo: repo, jobID: jobID, workers: 1}
}

// FetchLogs implements Source.
func (s *APISource) FetchLogs(ctx context.Context) ([]JobLog, RunInfo, error) {
	if s.jobID != 0 {
		return s.fetchSingleJob(ctx)
	}
	return s.fetchRun(ctx)
}

func (s *APISource) fetchSingleJob(ctx context.Context) ([]JobLog, RunInfo, error) {
	job, err := s.client.GetJob(ctx, s.owner, s.repo, s.jobID)
	if err != nil {
		return nil, RunInfo{}, err
	}
	info := RunInfo{
		RunID:      s.jobID,
		Title:      job.Name,
		Conclusion: job.Conclusion,
		TotalJobs:  1,
	}
	if failedConclusions[job.Conclusion] {
		info.FailedJobs = 1
	}
	content, err := s.client.JobLogs(ctx, s.owner, s.repo, s.jobID)
	if err == githubapi.ErrLogsExpired {
		info.ExpiredLogs = 1
		info.Warnings = append(info.Warnings, "job logs expired (older than ~90 days), nothing to analyze")
		return nil, info, nil
	}
	if err != nil {
		return nil, info, err
	}
	info.LogCount, info.LogBytes = 1, len(content)
	logs := []JobLog{{JobID: job.ID, JobName: job.Name, Content: mask.Content(content), Size: len(content)}}
	return logs, info, nil
}

func (s *APISource) fetchRun(ctx context.Context) ([]JobLog, RunInfo, error) {
	run, err := s.client.GetRun(ctx, s.owner, s.repo, s.runID)
	if err != nil {
		return nil, RunInfo{}, err
	}
	jobs, err := s.client.ListJobs(ctx, s.owner, s.repo, s.runID)
	if err != nil {
		return nil, RunInfo{}, err
	}

	info := RunInfo{
		RunID:      run.ID,
		RunNumber:  run.RunNumber,
		Workflow:   run.Name,
		Title:      run.DisplayTitle,
		Conclusion: run.Conclusion,
		HTMLURL:    run.HTMLURL,
		TotalJobs:  len(jobs),
	}
	var targets []githubapi.Job
	for _, j := range jobs {
		if s.allJobs || failedConclusions[j.Conclusion] {
			targets = append(targets, j)
			if failedConclusions[j.Conclusion] {
				info.FailedJobs++
			}
		}
	}
	if len(targets) == 0 {
		return nil, info, nil
	}

	logs := make([]JobLog, len(targets))
	errs := make([]error, len(targets))
	sem := make(chan struct{}, s.workers)
	var mu sync.Mutex // guards info.ExpiredLogs / info.Warnings
	var wg sync.WaitGroup
	for i, job := range targets {
		wg.Add(1)
		go func(i int, job githubapi.Job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content, err := s.client.JobLogs(ctx, s.owner, s.repo, job.ID)
			switch {
			case err == nil:
				logs[i] = JobLog{JobID: job.ID, JobName: job.Name, Content: mask.Content(content), Size: len(content)}
			case err == githubapi.ErrLogsExpired:
				errs[i] = nil
				mu.Lock()
				info.ExpiredLogs++
				info.Warnings = append(info.Warnings,
					fmt.Sprintf("logs expired for job %q (%d/%d available)", job.Name, len(targets)-info.ExpiredLogs, len(targets)))
				mu.Unlock()
			default:
				errs[i] = fmt.Errorf("job %q: %w", job.Name, err)
			}
		}(i, job)
	}
	wg.Wait()

	out := make([]JobLog, 0, len(logs))
	var hardErrs []error
	for i, l := range logs {
		if l.Content != "" || (l.JobName != "" && errs[i] == nil) {
			out = append(out, l)
			info.LogCount++
			info.LogBytes += l.Size
		}
		if errs[i] != nil {
			hardErrs = append(hardErrs, errs[i])
		}
	}
	// Every download failed — that is a utility error, not a degraded report.
	if len(out) == 0 && len(hardErrs) > 0 {
		return nil, info, hardErrs[0]
	}
	return out, info, nil
}

// FileSource reads local log files; each file counts as one job named after
// the file.
type FileSource struct {
	Paths []string
}

// NewFiles builds a local-file source.
func NewFiles(paths []string) *FileSource {
	return &FileSource{Paths: paths}
}

// FetchLogs implements Source.
func (s *FileSource) FetchLogs(ctx context.Context) ([]JobLog, RunInfo, error) {
	var logs []JobLog
	info := RunInfo{Title: "local files", TotalJobs: len(s.Paths)}
	for _, p := range s.Paths {
		if err := ctx.Err(); err != nil {
			return nil, info, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, info, fmt.Errorf("read %s: %w", p, err)
		}
		content := mask.Content(string(data))
		logs = append(logs, JobLog{JobName: filepath.Base(p), Content: content, Size: len(data)})
		info.LogCount++
		info.LogBytes += len(data)
	}
	return logs, info, nil
}
