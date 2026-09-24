package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunLocalFilesJestDupes(t *testing.T) {
	code, out, errOut := runForTest(t, "testdata/react_jest.log")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "[42×]") {
		t.Errorf("expected [42×] badge in output:\n%s", out)
	}
	if !strings.Contains(out, "43 errors → 1 groups:") {
		t.Errorf("expected grouped summary, got:\n%s", out)
	}
}

func TestRunGrafanaCaseCrossJob(t *testing.T) {
	code, out, errOut := runForTest(t,
		"testdata/grafana_lint.log", "testdata/grafana_test.log")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "[2 jobs]") {
		t.Errorf("expected [2 jobs] badge, got:\n%s", out)
	}
	if !strings.Contains(out, "error: pathspec <path> did not match any file(s) known to git") {
		t.Errorf("expected pathspec signature, got:\n%s", out)
	}
	if !strings.Contains(out, "Primary error (hypothesis): group 1 — found in 2 of 2 jobs.") {
		t.Errorf("expected primary hypothesis line, got:\n%s", out)
	}
}

func TestRunWerrorTrap(t *testing.T) {
	// 30 g++ command lines with -Werror must not become a group; the real
	// linking error must.
	code, out, errOut := runForTest(t, "--format", "json", "--min-group", "1", "testdata/node_werror.log")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var rep struct {
		Summary struct {
			ErrorLines int `json:"error_lines"`
		} `json:"summary"`
		Groups []struct {
			Signature string `json:"signature"`
			Count     int    `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(rep.Groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(rep.Groups), rep.Groups)
	}
	if !strings.Contains(rep.Groups[0].Signature, "linking") {
		t.Errorf("unexpected signature %q", rep.Groups[0].Signature)
	}
	if rep.Summary.ErrorLines != 1 {
		t.Errorf("error_lines = %d, want 1 (-Werror lines must not score)", rep.Summary.ErrorLines)
	}
}

func TestRunSingleFailureDegrades(t *testing.T) {
	code, out, errOut := runForTest(t, "testdata/single_failure.log")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "no repeating groups") {
		t.Errorf("expected degradation notice, got:\n%s", out)
	}
	if !strings.Contains(out, "panic: configuration is invalid: missing publish target") {
		t.Errorf("expected first error line, got:\n%s", out)
	}
}

func TestRunJSONIsMachineReadable(t *testing.T) {
	code, out, errOut := runForTest(t, "--format", "json", "testdata/grafana_lint.log")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out)
	}
	if rep["mode"] != "files" {
		t.Errorf("mode = %v", rep["mode"])
	}
}

func TestRunMDFile(t *testing.T) {
	mdPath := filepath.Join(t.TempDir(), "out.md")
	code, _, errOut := runForTest(t, "--md", mdPath, "testdata/single_failure.log")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	data, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "## ci-brief: ") {
		t.Errorf("md file header missing:\n%s", data)
	}
}

func TestRunTokenNeverLeaksIntoReport(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "leak.log")
	token := "ghp_0123456789abcdefghijklmnopqrstuv"
	content := "2026-09-24T12:00:00.0000000Z ##[error]error: push rejected to https://" + token + "@github.com/o/r.git (pkg/auth.ts:84)\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Degraded mode renders the first error line; JSON carries first_line.
	code, out, _ := runForTest(t, logPath)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, token) {
		t.Errorf("token leaked into report:\n%s", out)
	}
	if !strings.Contains(out, "gh***") {
		t.Errorf("expected masked token in report:\n%s", out)
	}

	code, out, _ = runForTest(t, "--format", "json", logPath)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, token) || !strings.Contains(out, "gh***") {
		t.Errorf("token leak in json report:\n%s", out)
	}
}

func TestRunErrorsExit2(t *testing.T) {
	if code, _, _ := runForTest(t, "testdata/nope_missing.log"); code != 2 {
		t.Errorf("missing file: exit = %d, want 2", code)
	}
	if code, _, errOut := runForTest(t, "--format", "yaml", "testdata/single_failure.log"); code != 2 || !strings.Contains(errOut, "format") {
		t.Errorf("bad format: exit = %d stderr = %s", code, errOut)
	}
	if code, _, errOut := runForTest(t, "--run", "1", "--job", "2"); code != 2 || !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("conflict: exit = %d stderr = %s", code, errOut)
	}
	if code, _, errOut := runForTest(t, "--run", "5", "testdata/single_failure.log"); code != 2 || !strings.Contains(errOut, "cannot be combined") {
		t.Errorf("files+run: exit = %d stderr = %s", code, errOut)
	}
}

func TestRunEmptyLogIsZero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runForTest(t, p)
	if code != 0 {
		t.Errorf("exit = %d, want 0 (nothing to analyze)", code)
	}
	if !strings.Contains(out, "no errors found") {
		t.Errorf("expected 'no errors found', got:\n%s", out)
	}
}

func TestParseArgsDefaults(t *testing.T) {
	o, err := parseArgs([]string{"a.txt", "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if o.format != "tty" || o.minGroup != 2 || o.top != 8 || o.run != -1 || o.job != -1 {
		t.Errorf("defaults wrong: %+v", o)
	}
	if len(o.files) != 2 {
		t.Errorf("files = %v", o.files)
	}

	o, err = parseArgs([]string{"--repo=owner/name", "--format", "json", "--top=3"})
	if err != nil {
		t.Fatal(err)
	}
	if o.repo != "owner/name" || o.format != "json" || o.top != 3 {
		t.Errorf("inline values wrong: %+v", o)
	}
}

func TestSplitRepo(t *testing.T) {
	tests := []struct {
		in    string
		owner string
		name  string
		ok    bool
	}{
		{"owner/name", "owner", "name", true},
		{"https://github.com/grafana/grafana.git", "grafana", "grafana", true},
		{"git@github.com:wrinfotel/ci-brief.git", "wrinfotel", "ci-brief", true},
		{"ssh://git@github.com/o/r", "o", "r", true},
		{"https://gitlab.company.com/team/proj.git", "team", "proj", true},
		{"justname", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		owner, name, ok := splitRepo(tt.in)
		if ok != tt.ok || owner != tt.owner || name != tt.name {
			t.Errorf("splitRepo(%q) = %q,%q,%v; want %q,%q,%v", tt.in, owner, name, ok, tt.owner, tt.name, tt.ok)
		}
	}
}

func TestGitRemoteFallback(t *testing.T) {
	orig := gitRemoteOrigin
	t.Cleanup(func() { gitRemoteOrigin = orig })
	gitRemoteOrigin = func() (string, error) {
		return "git@github.com:wrinfotel/ci-brief.git", nil
	}

	code, out, errOut := runForTest(t, "--format", "json")
	// No network in unit tests: we expect exit 2 from the API call, but the
	// repo must have been resolved from the remote (error mentions the run
	// lookup, not "no --repo given").
	if code == 2 && strings.Contains(errOut, "no --repo given") {
		t.Errorf("repo was not inferred from git remote: %s", errOut)
	}
	if strings.Contains(out, "wrinfotel") {
		t.Logf("unexpected success: %s", out)
	}
}
