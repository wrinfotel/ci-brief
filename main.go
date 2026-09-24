// Command ci-brief turns a failed GitHub Actions run into a short report
// with grouped errors: download the failed jobs' logs, normalize lines,
// group identical signatures and point at the likely primary cause.
//
// Exit codes: 0 — report produced or nothing to analyze; 2 — utility error
// (no network, no access, 404, bad flags). The failed run itself is not an
// error: ci-brief describes failures, it does not gate them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wrinfotel/ci-brief/internal/githubapi"
	"github.com/wrinfotel/ci-brief/internal/logsource"
	"github.com/wrinfotel/ci-brief/internal/report"
	"github.com/wrinfotel/ci-brief/internal/signature"
)

// version is overridden at release builds via -ldflags "-X main.version=…".
var version = "0.1.0"

const usage = `ci-brief — turn a failed GitHub Actions run into a short grouped report

Usage:
  ci-brief [flags] [files...]

  ci-brief                             last failed run of the origin repo
  ci-brief --repo owner/name --latest  last failed run
  ci-brief --repo owner/name --run 1842
  ci-brief --job 123456789             one job (repo from --repo or git remote)
  ci-brief log1.txt log2.txt           analyze local log files

Flags:
  --repo owner/name     target repository (default: git remote origin)
  --run ID              analyze this workflow run
  --job ID              analyze a single job
  --latest              pick the latest failed run (default without --run/--job)
  --format tty|md|json  output format (default tty)
  --md FILE             also write the markdown report to FILE
  --token TOKEN         GitHub token (default: $GITHUB_TOKEN)
  --min-group N         minimum lines for a group (default 2)
  --top N               show at most N groups (default 8)
  --all-jobs            analyze all jobs, not only the failed ones
  --version             print version
  -h, --help            show this help

Exit codes: 0 — report produced (or nothing to analyze); 2 — utility error.
`

type options struct {
	repo     string
	token    string
	format   string
	mdFile   string
	run      int64
	job      int64
	latest   bool
	allJobs  bool
	minGroup int
	top      int
	help     bool
	version  bool
	files    []string
}

// gitRemoteOrigin is a variable so tests can stub it.
var gitRemoteOrigin = func() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("cannot read git remote origin: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "ci-brief: %v\n", err)
		return 2
	}
	if opts.help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if opts.version {
		fmt.Fprintf(stdout, "ci-brief %s\n", version)
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := githubapi.New(opts.token)
	client.Progress = func(format string, a ...any) {
		fmt.Fprintf(stderr, "ci-brief: "+format+"\n", a...)
	}

	src, mode, repo, err := resolveSource(ctx, client, opts, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ci-brief: %v\n", err)
		return 2
	}

	logs, info, err := src.FetchLogs(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "ci-brief: %v\n", err)
		return 2
	}

	lines := make([]signature.Line, 0, 1024)
	for _, l := range logs {
		for _, text := range strings.Split(l.Content, "\n") {
			lines = append(lines, signature.Line{Job: l.JobName, Text: text})
		}
	}
	res := signature.Build(lines, opts.minGroup, opts.top)

	data := &report.Data{Mode: mode, Repo: repo, Info: info, Res: res}
	if err := render(data, opts, stdout); err != nil {
		fmt.Fprintf(stderr, "ci-brief: %v\n", err)
		return 2
	}
	return 0
}

func render(data *report.Data, opts *options, stdout io.Writer) error {
	switch opts.format {
	case "json":
		b, err := report.JSON(data)
		if err != nil {
			return err
		}
		stdout.Write(b)
	case "md":
		fmt.Fprint(stdout, report.MD(data))
	default:
		fmt.Fprint(stdout, report.TTY(data, useColor()))
	}
	if opts.mdFile != "" {
		f, err := os.Create(opts.mdFile)
		if err != nil {
			return fmt.Errorf("create %s: %w", opts.mdFile, err)
		}
		defer f.Close()
		if _, err := f.WriteString(report.MD(data)); err != nil {
			return fmt.Errorf("write %s: %w", opts.mdFile, err)
		}
	}
	return nil
}

func useColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	// TODO: switch to os.Stdout.Stat() char-device check without test interference.
	return isTerminal(os.Stdout)
}

// resolveSource wires the CLI selectors to a logsource.
func resolveSource(ctx context.Context, client *githubapi.Client, opts *options, stderr io.Writer) (logsource.Source, string, string, error) {
	if len(opts.files) > 0 {
		if opts.repo != "" || opts.run >= 0 || opts.job >= 0 {
			return nil, "", "", errors.New("local files cannot be combined with --repo/--run/--job")
		}
		return logsource.NewFiles(opts.files), report.ModeFiles, "", nil
	}

	owner, name, err := resolveRepo(opts)
	if err != nil {
		return nil, "", "", err
	}
	repo := owner + "/" + name

	switch {
	case opts.job >= 0:
		return logsource.NewAPIJob(client, owner, name, opts.job), report.ModeJob, repo, nil
	case opts.run >= 0:
		return logsource.NewAPI(client, owner, name, opts.run, opts.allJobs), report.ModeRun, repo, nil
	default:
		// --latest explicit or implied: no run/job selector given.
		fmt.Fprintf(stderr, "ci-brief: looking up the latest failed run of %s...\n", repo)
		r, err := client.LatestFailedRun(ctx, owner, name)
		if err != nil {
			return nil, "", "", err
		}
		fmt.Fprintf(stderr, "ci-brief: run #%d %q (%s)\n", r.RunNumber, r.DisplayTitle, r.Conclusion)
		return logsource.NewAPI(client, owner, name, r.ID, opts.allJobs), report.ModeRun, repo, nil
	}
}

// resolveRepo returns owner/name from --repo or the git remote origin.
func resolveRepo(opts *options) (string, string, error) {
	raw := opts.repo
	if raw == "" {
		remote, err := gitRemoteOrigin()
		if err != nil {
			return "", "", errors.New("no --repo given and cannot infer it from git remote origin")
		}
		raw = remote
	}
	owner, name, ok := splitRepo(raw)
	if !ok {
		return "", "", fmt.Errorf("cannot parse repository %q, want owner/name", raw)
	}
	return owner, name, nil
}

// splitRepo extracts owner/name from "o/n", https and ssh remote URLs.
func splitRepo(raw string) (string, string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimPrefix(s, "git@")
	// git@github.com:owner/repo
	if i := strings.IndexByte(s, ':'); i >= 0 && !strings.Contains(s, "://") {
		s = s[i+1:]
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	owner, name := parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

func parseArgs(args []string) (*options, error) {
	o := &options{run: -1, job: -1, format: "tty", minGroup: 2, top: 8}

	value := func(i *int, name, inline string) (string, error) {
		if inline != "" {
			return inline, nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("flag --%s requires a value", name)
		}
		*i++
		return args[*i], nil
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.files = append(o.files, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			name, inline, _ := strings.Cut(a[2:], "=")
			var err error
			switch name {
			case "repo":
				o.repo, err = value(&i, name, inline)
			case "token":
				o.token, err = value(&i, name, inline)
			case "format":
				o.format, err = value(&i, name, inline)
			case "md":
				o.mdFile, err = value(&i, name, inline)
			case "run":
				var s string
				if s, err = value(&i, name, inline); err == nil {
					o.run, err = strconv.ParseInt(s, 10, 64)
				}
			case "job":
				var s string
				if s, err = value(&i, name, inline); err == nil {
					o.job, err = strconv.ParseInt(s, 10, 64)
				}
			case "min-group":
				var s string
				if s, err = value(&i, name, inline); err == nil {
					o.minGroup, err = strconv.Atoi(s)
				}
			case "top":
				var s string
				if s, err = value(&i, name, inline); err == nil {
					o.top, err = strconv.Atoi(s)
				}
			case "latest", "all-jobs", "help":
				err = setBool(o, name, inline)
			case "version":
				o.version = true
			default:
				err = fmt.Errorf("unknown flag --%s", name)
			}
			if err != nil {
				return nil, err
			}
		case a == "-h":
			o.help = true
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown flag %s", a)
		default:
			o.files = append(o.files, a)
		}
	}

	if o.token == "" {
		o.token = os.Getenv("GITHUB_TOKEN")
	}
	switch o.format {
	case "tty", "md", "json":
	default:
		return nil, fmt.Errorf("unknown --format %q (want tty, md or json)", o.format)
	}
	if o.minGroup < 1 {
		return nil, errors.New("--min-group must be >= 1")
	}
	if o.top < 1 {
		return nil, errors.New("--top must be >= 1")
	}
	if o.run >= 0 && o.job >= 0 {
		return nil, errors.New("--run and --job are mutually exclusive")
	}
	if o.latest && o.run >= 0 {
		return nil, errors.New("--latest and --run are mutually exclusive")
	}
	if o.latest && o.job >= 0 {
		return nil, errors.New("--latest and --job are mutually exclusive")
	}
	return o, nil
}

func setBool(o *options, name, inline string) error {
	v := true
	if inline != "" {
		b, err := strconv.ParseBool(inline)
		if err != nil {
			return fmt.Errorf("flag --%s: %w", name, err)
		}
		v = b
	}
	switch name {
	case "latest":
		o.latest = v
	case "all-jobs":
		o.allJobs = v
	case "help":
		o.help = v
	}
	return nil
}

// isTerminal reports whether f is a character device (tty).
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
