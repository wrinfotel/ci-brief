package signature

import (
	"strings"
	"testing"
)

func TestScoreClassification(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		isErr  bool
		contxt string // expected group signature substring when isErr
	}{
		// Plan §6: -Werror is NOT an error; pathspec IS; g++ command is NOT.
		{"compiler flag", "gcc -Werror -c foo.c -o foo.o", false, ""},
		{"gxx command", "g++ -std=c++17 -Wall main.cpp", false, ""},
		{"node command", "node scripts/build.mjs --mode=production", false, ""},
		{"git command", "git submodule update --init --recursive", false, ""},
		{"pathspec error", "error: pathspec 'none' did not match any file(s) known to git", true, "error: pathspec <path> did not match any file(s) known to git"},
		{"typeerror", "TypeError: Cannot read properties of undefined (reading 'length')", true, "TypeError: Cannot read properties of undefined (reading <path>)"},
		{"npm err", "npm ERR! code E403", true, "npm ERR! code E403"},
		{"npm lowercase error", "npm error code E403", true, "npm error code E403"},
		{"gcc reporting error", "gcc: fatal error: stdio.h: No such file or directory", true, "gcc: fatal error: stdio.h: No such file or directory"},
		{"go vet failure", "vet: ./api/client.go:42: undefined: cfg", false, ""},
		{"comment noise", "# replicas: 2 in values.yaml error", false, ""},
		{"retry noise", "Retrying download after failure (attempt 2/5)", false, ""},
		{"panic", "panic: runtime error: invalid memory address or nil pointer dereference", true, "panic: runtime error: invalid memory address or nil pointer dereference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, _ := Score(tt.line)
			if got := score >= 2; got != tt.isErr {
				t.Errorf("Score(%q) = %d, want isErr=%v", tt.line, score, tt.isErr)
			}
		})
	}
}

func TestBuildJestDuplicatesOneGroup(t *testing.T) {
	// Plan §6: 42 identical jest hints -> one group.
	var lines []Line
	for i := 0; i < 42; i++ {
		lines = append(lines, Line{Job: "jest", Text: "TypeError: Cannot read properties of undefined (reading 'map')"})
	}
	res := Build(lines, 2, 8)
	if len(res.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(res.Groups))
	}
	g := res.Groups[0]
	if g.Count != 42 {
		t.Errorf("count = %d, want 42", g.Count)
	}
	if len(g.Jobs) != 1 || g.Jobs[0] != "jest" {
		t.Errorf("jobs = %v, want [jest]", g.Jobs)
	}
	if res.Primary == nil {
		t.Error("count 42 >= 3 must yield primary candidate")
	}
}

func TestBuildSeparatorVariantsMerge(t *testing.T) {
	// Plan §6: signatures with and without the table separator -> one group.
	lines := []Line{
		{Job: "a", Text: "FAIL some · package · tests"},
		{Job: "a", Text: "FAIL some package tests"},
		{Job: "b", Text: "FAIL  some  package  tests"},
	}
	res := Build(lines, 2, 8)
	if len(res.Groups) != 1 {
		t.Fatalf("groups = %d, want 1 merged group, got %+v", len(res.Groups), res.Groups)
	}
	if res.Groups[0].Count != 3 || len(res.Groups[0].Jobs) != 2 {
		t.Errorf("want count=3 jobs=2, got %d %v", res.Groups[0].Count, res.Groups[0].Jobs)
	}
}

func TestBuildCrossJobCascade(t *testing.T) {
	// Spike flagship shape: pathspec in 2 of 3 jobs, one-off errors elsewhere.
	pathspec := "##[error]error: pathspec 'none' did not match any file(s) known to git"
	var lines []Line
	for _, job := range []string{"lint", "test-unit", "test-e2e"} {
		lines = append(lines,
			Line{Job: job, Text: "##[group]Run git checkout"},
			Line{Job: job, Text: pathspec},
			Line{Job: job, Text: "##[error]Process completed with exit code 1."},
		)
	}
	lines = append(lines,
		Line{Job: "build-docs", Text: "fatal: repository 'docs' not found"},
	)
	res := Build(lines, 2, 8)
	if len(res.Groups) != 1 {
		t.Fatalf("groups = %d (%v), want 1 (one-off fatal line is below min-group)", len(res.Groups), groupSignatures(res.Groups))
	}
	if res.ErrorLines != 4 {
		t.Errorf("ErrorLines = %d, want 4 (3 pathspec + 1 fatal)", res.ErrorLines)
	}
	first := res.Groups[0]
	if !strings.Contains(first.Signature, "pathspec") {
		t.Errorf("primary sort wrong: first group is %q", first.Signature)
	}
	if len(first.Jobs) != 3 {
		t.Errorf("pathspec jobs = %d, want 3", len(first.Jobs))
	}
	if res.Primary != first {
		t.Error("pathspec group must be the cascade primary")
	}
	// "first" must keep the un-normalized (cleaned) original.
	if first.FirstOriginal != "error: pathspec 'none' did not match any file(s) known to git" {
		t.Errorf("FirstOriginal = %q", first.FirstOriginal)
	}
}

func TestBuildMinGroupAndDegrade(t *testing.T) {
	lines := []Line{
		{Job: "single", Text: "panic: configuration is invalid"},
	}
	res := Build(lines, 2, 8)
	if !res.Degraded {
		t.Fatal("single error below min-group must degrade")
	}
	if res.Groups != nil && len(res.Groups) != 0 {
		t.Errorf("degraded result must have no groups, got %v", res.Groups)
	}
	if res.FirstError != "panic: configuration is invalid" {
		t.Errorf("FirstError = %q", res.FirstError)
	}
	if res.ErrorLines != 1 {
		t.Errorf("ErrorLines = %d, want 1", res.ErrorLines)
	}
}

func TestBuildTopCut(t *testing.T) {
	var lines []Line
	for i := 0; i < 10; i++ {
		text := "panic: unique error " + strings.Repeat("x", i+1)
		lines = append(lines, Line{Job: "j1", Text: text}, Line{Job: "j2", Text: text})
	}
	res := Build(lines, 2, 3)
	if len(res.Groups) != 3 {
		t.Errorf("top cut: groups = %d, want 3", len(res.Groups))
	}
	for _, g := range res.Groups {
		if g.Count != 2 || len(g.Jobs) != 2 {
			t.Errorf("group %q: count=%d jobs=%v", g.Signature, g.Count, g.Jobs)
		}
	}
}

func TestExtractFile(t *testing.T) {
	if got := extractFile("at packages/api/src/auth.ts:84 in run"); got != "packages/api/src/auth.ts:84" {
		t.Errorf("file:line = %q", got)
	}
	if got := extractFile("TypeError: Cannot read properties of undefined"); got != "" {
		t.Errorf("bare file on message without path = %q, want empty", got)
	}
}

func groupSignatures(gs []*Group) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Signature)
	}
	return out
}
