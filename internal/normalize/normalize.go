// Package normalize cleans CI log lines into stable signatures.
//
// The step order below is load-bearing (validated by the 24.09.2026 spike
// against logs from 15 large repositories) — do not reorder:
//
//  1. ANSI escape sequences
//  2. GitHub Actions timestamp prefix (2026-09-24T17:55:59.123Z )
//  3. Generic time prefix ([17:55:59] / 17:55:59.)
//  4. ##[group] / ##[endgroup] / ##[error] action markers
//  5. UUID -> <uuid>, IPv4 -> <ip>
//  6. Numbers with units -> <num>, lone numbers -> <n> (word/dot boundaries kept)
//  7. Paths -> <path>
//  8. Table separators, whitespace collapse
//
// Go's regexp (RE2) has no lookbehind/lookahead, so the boundary rules from
// the plan are enforced by checking neighboring bytes at match positions.
package normalize

import (
	"regexp"
	"strings"
)

var (
	ansiCSI     = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	ansiOSC     = regexp.MustCompile(`\x1b\][^\x07]*\x07`)
	ansiCharset = regexp.MustCompile(`\x1b\(B`)

	ghTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z ?`)
	timePrefix  = regexp.MustCompile(`^\[?\d{2}:\d{2}:\d{2}\]?[. ]?`)

	actionMarker = regexp.MustCompile(`##\[(?:group|endgroup|error)\] ?`)

	uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	ipv4Re = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

	numWithUnit = regexp.MustCompile(`\d+(?:\.\d+)? ?(?:ms|us|µs|ns|min|KB|MB|GB|TB|kB|kb|mb|gb|m|s|h|B)`)
	loneNumber  = regexp.MustCompile(`\d+(?:\.\d+)?`)

	pathRe = regexp.MustCompile(`[^\s:]*[/\\][^\s:]*`)

	whitespace = regexp.MustCompile(`[ \t]+`)
)

// isBoundaryByte reports whether b continues a word or a decimal number:
// letters, digits, underscore and dot protect things like v1.2.3 and identifiers.
func isBoundaryByte(b byte) bool {
	return b == '_' || b == '.' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// Line turns a raw log line into its normalized signature form.
func Line(raw string) string {
	s := raw

	// 1. ANSI escapes.
	s = ansiCSI.ReplaceAllString(s, "")
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCharset.ReplaceAllString(s, "")

	// 2. GitHub Actions log timestamp.
	s = ghTimestamp.ReplaceAllString(s, "")

	// 3. Generic time prefix.
	s = timePrefix.ReplaceAllString(s, "")

	// 4. Action markers: strip the marker, keep the payload.
	s = actionMarker.ReplaceAllString(s, "")

	// 5. UUIDs and IPv4 before the number passes can eat them.
	s = uuidRe.ReplaceAllString(s, "<uuid>")
	s = ipv4Re.ReplaceAllString(s, "<ip>")

	// 6a. Numbers with units.
	s = numWithUnit.ReplaceAllString(s, "<num>")

	// 6b. Lone numbers, protected when glued to word/dot characters.
	s = replaceAtBoundaries(s, loneNumber, "<n>", false)

	// 7. Paths — the most aggressive step; the left boundary keeps
	// "a/b" glued to words from being rewritten.
	s = replaceAtBoundaries(s, pathRe, "<path>", true)

	// 8. Table separators and whitespace collapse.
	s = strings.Map(func(r rune) rune {
		if r == '·' || r == '•' {
			return ' '
		}
		return r
	}, s)
	s = whitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// replaceAtBoundaries substitutes re matches with repl, skipping matches
// whose left neighbor is a word character (and, for paths, a slash), or —
// for numbers — whose right neighbor is a word/dot character.
func replaceAtBoundaries(s string, re *regexp.Regexp, repl string, pathMode bool) string {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		b.WriteString(s[last:start])
		if boundaryProtected(s, start, end, pathMode) {
			b.WriteString(s[start:end])
		} else {
			b.WriteString(repl)
		}
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func boundaryProtected(s string, start, end int, pathMode bool) bool {
	if start > 0 {
		prev := s[start-1]
		if isBoundaryByte(prev) {
			return true
		}
		if pathMode && (prev == '/' || prev == '\\') {
			return true
		}
	}
	if !pathMode && end < len(s) && isBoundaryByte(s[end]) {
		return true
	}
	return false
}

// Clean prepares a raw line for display ("first:" lines): it strips log
// plumbing (steps 1–4) but keeps paths, numbers and message content intact.
func Clean(raw string) string {
	s := strings.TrimRight(raw, "\r")
	s = ansiCSI.ReplaceAllString(s, "")
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCharset.ReplaceAllString(s, "")
	s = ghTimestamp.ReplaceAllString(s, "")
	s = timePrefix.ReplaceAllString(s, "")
	s = actionMarker.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// Signature returns the grouping key for a normalized line: truncated to
// 200 runes so multi-KB messages collapse into one bucket.
func Signature(normalized string) string {
	runes := 0
	for i := range normalized {
		if runes == 200 {
			return normalized[:i]
		}
		runes++
	}
	return normalized
}
