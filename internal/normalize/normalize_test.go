package normalize

import (
	"strings"
	"testing"
)

func TestLineGolden(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ansi color", "\x1b[31merror\x1b[0m: boom", "error: boom"},
		{"ansi osc + charset", "\x1b]0;title\x07\x1b(Berror: boom", "error: boom"},
		{"gh timestamp + marker", "2026-09-24T17:55:59.1234567Z ##[error]Process completed with exit code 1.",
			"Process completed with exit code 1."},
		{"bracket time prefix", "[17:55:59] build failed", "build failed"},
		{"dot time prefix", "17:55:59.123 connect ECONNRESET", "<n> connect ECONNRESET"},
		{"group marker keeps line", "##[group]Run actions/checkout@v4", "Run <path>"},
		{"error marker payload", "##[error]Something broke", "Something broke"},
		{"endgroup empties", "##[endgroup]", ""},
		{"uuid", "corrupted id 550e8400-e29b-41d4-a716-446655440000 retry", "corrupted id <uuid> retry"},
		{"ipv4", "GET http://10.0.0.1/api from client", "GET http:<path> from client"},
		{"units", "Build took 5s, wrote 1.5MB, waited 3 min", "Build took <num>, wrote <num>, waited <num>"},
		{"version protected", "node v20.5.0 exited with code 1", "node v20.5.0 exited with code <n>"},
		{"path with line", "at packages/api/src/auth.ts:84 in run", "at <path>:<n> in run"},
		{"windows path", "cannot find C:\\tools\\make.exe here", "cannot find C:<path> here"},
		{"relative path", "cannot read ./x/y config", "cannot read <path> config"},
		{"separators collapse", "pass  a · b•c   d", "pass a b c d"},
		{"werror untouched", "gcc -Werror -c foo.c", "gcc -Werror -c foo.c"},
		{"id underscore protected", "job_id_42 done", "job_id_42 done"},
		{"dot protected", "code 1. and 2.", "code 1. and 2."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Line(tt.in); got != tt.want {
				t.Errorf("Line(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanKeepsContent(t *testing.T) {
	in := "2026-09-24T17:55:59.123Z \x1b[31m##[error]packages/api/src/auth.ts:84\x1b[0m"
	want := "packages/api/src/auth.ts:84"
	if got := Clean(in); got != want {
		t.Errorf("Clean(%q) = %q, want %q", in, got, want)
	}
}

func TestSignatureTruncatesTo200Runes(t *testing.T) {
	long := strings.Repeat("ж", 300)
	got := Signature(long)
	if n := len([]rune(got)); n != 200 {
		t.Errorf("Signature length = %d runes, want 200", n)
	}
	if got != Signature(long) {
		t.Error("Signature must be deterministic")
	}
}

func TestLineDeterministic(t *testing.T) {
	in := "2026-09-24T17:55:59.123Z error: pathspec 'foo/bar.ts' did not match any file(s) known to git"
	a, b := Line(in), Line(in)
	if a != b {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
	if a != "error: pathspec <path> did not match any file(s) known to git" {
		t.Errorf("unexpected normalization: %q", a)
	}
}
