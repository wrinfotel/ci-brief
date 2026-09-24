package mask

import (
	"strings"
	"testing"
)

func TestContentGolden(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"github pat literal",
			"git clone https://x-access-token:ghp_0123456789abcdefghijklmnopqrstuv@github.com/o/r.git",
			"git clone https://x-access-token:gh***@github.com/o/r.git",
		},
		{
			"fine-grained pat",
			"github_pat_11AAAAAAA0abcdefghijklmnopq secret usage",
			"gh*** secret usage",
		},
		{
			"env assignment",
			"GITHUB_TOKEN=sup3rsecretvalue123 exported before run",
			"GITHUB_TOKEN=*** exported before run",
		},
		{
			"json style",
			`{"api_key": "abcdef123456", "n": 1}`,
			`{"api_key": "***", "n": 1}`,
		},
		{
			"authorization header",
			"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
			"Authorization: Bearer ***",
		},
		{
			"negative: token word without value",
			"error: token not found in cache",
			"error: token not found in cache",
		},
		{
			"negative: no keyword, long run stays",
			"checksum a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8 ok",
			"checksum a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8 ok",
		},
		{
			"long run near keyword",
			"verifying signing key a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8 failed",
			"verifying signing key *** failed",
		},
		{
			"short value stays",
			"cache key: ab12 in use",
			"cache key: ab12 in use",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Content(tt.in); got != tt.want {
				t.Errorf("Content(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Plan §6 acceptance: a token passed via GITHUB_TOKEN must never leak into
// the report, even when the log echoes it.
func TestEnvTokenNeverLeaks(t *testing.T) {
	token := "ghp_" + "Xy1234567890abcdefghij1234"
	line := "git push https://" + token + "@github.com/o/r.git"
	out := Content(line)
	if strings.Contains(out, token) {
		t.Fatalf("token leaked: %q", out)
	}
	if !strings.Contains(out, "gh***") {
		t.Errorf("expected gh*** mask, got %q", out)
	}
}
