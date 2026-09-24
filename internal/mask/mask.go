// Package mask scrubs secrets from log content and report output.
//
// Detection per the plan (§1):
//   - GitHub token literals (ghp_/gho_/ghs_/ghu_/ghr_, github_pat_) -> "gh***";
//   - values of token/secret/key/password-style assignments -> "***";
//   - any 20+ alphanumeric run on a line that also mentions
//     token/secret/key/password/credential -> "***".
package mask

import "regexp"

var (
	githubTokenRe = regexp.MustCompile(`(?i)\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{22,})`)

	// The value capture keeps the keyword, separator and quotes intact.
	// @, \ and * are excluded so URLs and pre-masked "gh***" values survive.
	envAssignRe = regexp.MustCompile(`(?i)((?:[A-Za-z0-9_-]*(?:token|secret|key|password|passwd|credentials?)[A-Za-z0-9_-]*)["']?\s*[:=]\s*["']?)([A-Za-z0-9+/=._\-~]{8,})`)

	bearerRe = regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[^\s]+`)

	secretWordRe = regexp.MustCompile(`(?i)token|secret|password|passwd|credential|\bkeys?\b|api[-_ ]?key|private[-_ ]?key`)
	longRunRe    = regexp.MustCompile(`[A-Za-z0-9]{20,}`)
)

// Content masks secrets in a log line or block.
func Content(s string) string {
	s = githubTokenRe.ReplaceAllString(s, "gh***")
	s = bearerRe.ReplaceAllString(s, "${1}***")
	s = envAssignRe.ReplaceAllString(s, "${1}***")
	if secretWordRe.MatchString(s) {
		s = longRunRe.ReplaceAllString(s, "***")
	}
	return s
}
