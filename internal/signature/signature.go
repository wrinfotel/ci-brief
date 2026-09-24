// Package signature scores log lines for "errorness", groups them by
// normalized signature and picks the cascade primary-cause candidate.
//
// Scoring rules from the plan (§5), validated by the 24.09.2026 spike:
//
//	+2  error as a word (left boundary mandatory, otherwise -Werror matches)
//	+2  FAIL / Failed / FAILURE / fatal / panic: / Exception / Traceback
//	+1  Cannot, Unable to, No such file, not found, Permission denied,
//	    ECONN, ETIMEDOUT, refused, exited with code
//
// Mandatory exceptions (−2 each):
//   - lines starting with a build command (compiler command echoes are not
//     errors); skipped when the line carries a real error token, so
//     "npm error code E403" / "npm ERR! code E403" (the tool *reporting* an
//     error, as grouped by the spike) still qualify while "g++ -Werror …"
//     does not;
//   - retrying/attempting/… CI progress noise at line start;
//   - config comments (^#).
//
// A line is an error line when its score is >= 2.
package signature

import (
	"regexp"
	"sort"
	"strings"

	"github.com/wrinfotel/ci-brief/internal/normalize"
)

// Line is one raw log line with the job it came from. Text must already be
// secret-masked by the caller.
type Line struct {
	Job  string
	Text string
}

// Group is one bucket of identical signatures.
type Group struct {
	Signature     string
	Count         int      // total lines in the group across all jobs
	Jobs          []string // distinct job names, sorted
	FirstOriginal string   // cleaned (not normalized) first occurrence
	FirstJob      string
	FirstFile     string // file:line extracted from FirstOriginal, "" if none
	firstIndex    int    // chronological order of first occurrence
}

// Result is the outcome of one analysis pass.
type Result struct {
	Groups     []*Group // sorted: jobs desc, count desc, first seen asc
	ErrorLines int
	TotalLines int
	Primary    *Group // cascade hypothesis; nil when none qualifies
	Degraded   bool   // errors exist but none reached min-group
	FirstError string // cleaned first error line, for degraded mode
}

var (
	failFamily = []string{"FAIL", "Failed", "FAILURE", "fatal", "panic:", "Exception", "Traceback"}
	weakFamily = []string{
		"Cannot", "Unable to", "No such file", "not found", "Permission denied",
		"ECONN", "ETIMEDOUT", "refused", "exited with code",
	}

	buildCmdRe = regexp.MustCompile(`^(sccache|cc|c\+\+|g\+\+|clang|gcc|ld|ar |npm |yarn|pnpm|go (?:build|test|vet)|cargo|cmake|make|python|node |jest|eslint|tsc|docker |git |rsync|pip )`)
	noiseRe    = regexp.MustCompile(`^(?i)(retrying|attempting|downloading|fetching|installing|building|compiling)`)
	errorTSRe  = regexp.MustCompile(`error TS\d+`)

	fileLineRe = regexp.MustCompile(`(?:[A-Za-z]:)?[^\s"'` + "`" + `]*[/\\][^\s"'` + "`" + `]*\.[A-Za-z0-9]+:\d+`)
	bareFileRe = regexp.MustCompile(`(?:[A-Za-z]:)?[^\s"'` + "`" + `]*[/\\][^\s"'` + "`" + `]*\.[A-Za-z0-9]+`)
)

func isWordByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// errorToken reports whether the line contains an error marker (+2 family)
// and whether that marker is real rather than a flag like "-error".
func errorToken(line string) (found, real bool) {
	lower := strings.ToLower(line)

	mark := func(pos int) {
		found = true
		if pos == 0 || line[pos-1] != '-' {
			real = true
		}
	}

	// [Ee]rror with both word boundaries; all-caps ERROR not followed by a letter.
	for i := 0; ; {
		j := strings.Index(lower[i:], "error")
		if j < 0 {
			break
		}
		j += i
		i = j + len("error")
		leftOK := j == 0 || !isWordByte(line[j-1])
		rightOK := i >= len(line) || !isWordByte(line[i])
		if leftOK && rightOK {
			mark(j)
		} else if i < len(line) && line[i] == ':' {
			mark(j) // "error:" / "Error:" payload form
		}
	}
	for i := 0; ; {
		j := strings.Index(line[i:], "ERROR")
		if j < 0 {
			break
		}
		j += i
		i = j + len("ERROR")
		if i >= len(line) || !isLetter(line[i]) {
			mark(j)
		}
	}
	for i := 0; ; {
		j := strings.Index(lower[i:], "err!")
		if j < 0 {
			break
		}
		j += i
		i = j + len("err!")
		if j == 0 || !isWordByte(line[j-1]) {
			mark(j)
		}
	}
	if m := errorTSRe.FindStringIndex(line); m != nil {
		mark(m[0])
	}
	return found, real
}

// Score returns the error score of an already-cleaned line and whether it
// carries a real (non-flag) error token.
func Score(line string) (int, bool) {
	score := 0

	found, real := errorToken(line)
	if found {
		score += 2
	}
	if hasAny(line, failFamily) {
		score += 2
	}

	// +1 weak signals, capped at +2.
	weak := 0
	for _, w := range weakFamily {
		if strings.Contains(line, w) {
			weak++
		}
	}
	if weak > 2 {
		weak = 2
	}
	score += weak

	// Exceptions, −2 each.
	if !real && buildCmdRe.MatchString(line) {
		score -= 2
	}
	if noiseRe.MatchString(line) {
		score -= 2
	}
	if strings.HasPrefix(line, "#") {
		score -= 2
	}
	return score, real
}

func hasAny(line string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(line, p) {
			return true
		}
	}
	return false
}

// Build groups error lines. minGroup is the minimum line count for a group
// to be reported; top limits how many groups are kept (<= 0 keeps all).
func Build(lines []Line, minGroup, top int) *Result {
	res := &Result{}
	type bucket struct {
		group  *Group
		jobSet map[string]bool
	}
	byKey := map[string]*bucket{}
	idx := 0

	for _, l := range lines {
		res.TotalLines++
		if strings.TrimSpace(l.Text) == "" {
			continue
		}
		cleaned := normalize.Clean(l.Text)
		score, _ := Score(cleaned)
		if score < 2 {
			continue
		}
		res.ErrorLines++
		idx++
		if res.FirstError == "" {
			res.FirstError = cleaned
		}

		key := normalize.Signature(normalize.Line(l.Text))
		b := byKey[key]
		if b == nil {
			b = &bucket{
				group: &Group{
					Signature:     key,
					FirstOriginal: cleaned,
					FirstJob:      l.Job,
					FirstFile:     extractFile(cleaned),
					firstIndex:    idx,
				},
				jobSet: map[string]bool{},
			}
			byKey[key] = b
		}
		b.group.Count++
		if !b.jobSet[l.Job] {
			b.jobSet[l.Job] = true
			b.group.Jobs = append(b.group.Jobs, l.Job)
		}
	}

	for _, b := range byKey {
		sort.Strings(b.group.Jobs)
		res.Groups = append(res.Groups, b.group)
	}

	// min-group filter; degrade when nothing survives but errors exist.
	filtered := make([]*Group, 0, len(res.Groups))
	for _, g := range res.Groups {
		if g.Count >= minGroup {
			filtered = append(filtered, g)
		}
	}
	res.Groups = filtered
	if len(res.Groups) == 0 && res.ErrorLines > 0 {
		res.Degraded = true
		return res
	}

	sort.SliceStable(res.Groups, func(i, j int) bool {
		gi, gj := res.Groups[i], res.Groups[j]
		if len(gi.Jobs) != len(gj.Jobs) {
			return len(gi.Jobs) > len(gj.Jobs)
		}
		if gi.Count != gj.Count {
			return gi.Count > gj.Count
		}
		return gi.firstIndex < gj.firstIndex
	})

	if top > 0 && len(res.Groups) > top {
		res.Groups = res.Groups[:top]
	}

	// Cascade hypothesis: the group with the most jobs (count >= 3) comes
	// first in sort order, ties broken by earliest first occurrence.
	for _, g := range res.Groups {
		if g.Count >= 3 {
			res.Primary = g
			break
		}
	}
	return res
}

// extractFile pulls a file:line (or bare path) out of a cleaned line.
func extractFile(cleaned string) string {
	if m := fileLineRe.FindString(cleaned); m != "" {
		return m
	}
	return bareFileRe.FindString(cleaned)
}
