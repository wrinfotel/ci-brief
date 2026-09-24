// Package report renders an analysis result as tty (colored), markdown or
// JSON output.
package report

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wrinfotel/ci-brief/internal/logsource"
	"github.com/wrinfotel/ci-brief/internal/signature"
)

// Mode describes what was analyzed.
const (
	ModeRun   = "run"
	ModeJob   = "job"
	ModeFiles = "files"
)

// Data bundles everything the renderers need.
type Data struct {
	Mode string // ModeRun / ModeJob / ModeFiles
	Repo string // "owner/repo", empty for local files
	Info logsource.RunInfo
	Res  *signature.Result
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

type jsonGroup struct {
	Signature string   `json:"signature"`
	Count     int      `json:"count"`
	JobCount  int      `json:"job_count"`
	Jobs      []string `json:"jobs"`
	FirstLine string   `json:"first_line,omitempty"`
	FirstJob  string   `json:"first_job,omitempty"`
	FirstFile string   `json:"first_file,omitempty"`
}

type jsonReport struct {
	Mode    string            `json:"mode"`
	Repo    string            `json:"repo,omitempty"`
	Run     logsource.RunInfo `json:"run"`
	Summary struct {
		TotalLines int  `json:"total_lines"`
		ErrorLines int  `json:"error_lines"`
		Groups     int  `json:"groups"`
		Degraded   bool `json:"degraded"`
	} `json:"summary"`
	Primary *jsonGroup  `json:"primary,omitempty"`
	Groups  []jsonGroup `json:"groups"`
	// FirstError is set in degraded mode, when no group reached min-group.
	FirstError string `json:"first_error,omitempty"`
}

// JSON renders the machine-readable report (jq-friendly).
func JSON(d *Data) ([]byte, error) {
	out := jsonReport{
		Mode: d.Mode,
		Repo: d.Repo,
		Run:  d.Info,
	}
	out.Summary.TotalLines = d.Res.TotalLines
	out.Summary.ErrorLines = d.Res.ErrorLines
	out.Summary.Groups = len(d.Res.Groups)
	out.Summary.Degraded = d.Res.Degraded
	out.FirstError = d.Res.FirstError
	out.Groups = make([]jsonGroup, 0, len(d.Res.Groups))
	for _, g := range d.Res.Groups {
		out.Groups = append(out.Groups, toJSONGroup(g))
	}
	if d.Res.Primary != nil {
		pg := toJSONGroup(d.Res.Primary)
		out.Primary = &pg
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func toJSONGroup(g *signature.Group) jsonGroup {
	jg := jsonGroup{
		Signature: g.Signature,
		Count:     g.Count,
		JobCount:  len(g.Jobs),
		Jobs:      g.Jobs,
		FirstLine: g.FirstOriginal,
		FirstJob:  g.FirstJob,
		FirstFile: g.FirstFile,
	}
	if jg.Jobs == nil {
		jg.Jobs = []string{}
	}
	return jg
}

// TTY renders the human report; color controls ANSI escapes.
func TTY(d *Data, color bool) string {
	var b strings.Builder
	if color {
		b.WriteString(ttyBody(d, colorize))
	} else {
		b.WriteString(ttyBody(d, plain))
	}
	return b.String()
}

type paint func(s, code string) string

func plain(s, _ string) string { return s }

func colorize(s, code string) string {
	return code + s + ansiReset
}

func ttyBody(d *Data, p paint) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", p(header(d), ansiBold))
	for _, w := range d.Info.Warnings {
		fmt.Fprintf(&b, "%s\n", p("warning: "+w, ansiYellow))
	}

	switch {
	case d.Res.Degraded:
		fmt.Fprintf(&b, "\n%s\n", p(fmt.Sprintf("%d error line(s), no repeating groups:", d.Res.ErrorLines), ansiRed))
		fmt.Fprintf(&b, "  %s\n", truncate(d.Res.FirstError, 200))
		return b.String()
	case len(d.Res.Groups) == 0:
		fmt.Fprintf(&b, "\n%s\n", p("no errors found in logs", ansiGreen))
		return b.String()
	}

	fmt.Fprintf(&b, "\n%s\n\n", p(fmt.Sprintf("%d errors → %d groups:", d.Res.ErrorLines, len(d.Res.Groups)), ansiRed))
	primaryIdx := 0
	for i, g := range d.Res.Groups {
		if d.Res.Primary != nil && g == d.Res.Primary {
			primaryIdx = i + 1
		}
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, p(groupBadge(g), ansiBold), g.Signature)
		if g.FirstFile != "" {
			fmt.Fprintf(&b, "   %s %s\n", p("first:", ansiDim), g.FirstFile)
		}
	}
	if d.Res.Primary != nil {
		fmt.Fprintf(&b, "\n%s\n", p(fmt.Sprintf(
			"Primary error (hypothesis): group %d — found in %d of %d jobs.",
			primaryIdx, len(d.Res.Primary.Jobs), d.Info.TotalJobs), ansiYellow))
	}
	return b.String()
}

// header builds the first report line, mirroring the plan example:
// "Run #1842 failed: 7 jobs, 45 logs, 2.3 MB".
func header(d *Data) string {
	stats := fmt.Sprintf("%d logs, %s", d.Info.LogCount, humanBytes(d.Info.LogBytes))
	switch d.Mode {
	case ModeJob:
		return fmt.Sprintf("Job %q (%s): %s", d.Info.Title, statusWord(d.Info.Conclusion), stats)
	case ModeFiles:
		return fmt.Sprintf("%d files: %s", d.Info.TotalJobs, stats)
	default:
		return fmt.Sprintf("Run #%d %s%s: %s", d.Info.RunNumber, statusWord(d.Info.Conclusion),
			workflowSuffix(d.Info.Workflow, d.Info.Title), stats)
	}
}

func statusWord(conclusion string) string {
	if conclusion == "success" || conclusion == "" {
		return "finished"
	}
	return conclusion
}

func workflowSuffix(workflow, title string) string {
	s := ""
	if workflow != "" {
		s += ": " + workflow
	}
	if title != "" && title != workflow {
		s += " — " + title
	}
	return s
}

// groupBadge mirrors the plan: multi-job groups show "[N jobs]", single-job
// repeat counts show "[count×]".
func groupBadge(g *signature.Group) string {
	if len(g.Jobs) > 1 {
		return fmt.Sprintf("[%d jobs]", len(g.Jobs))
	}
	return fmt.Sprintf("[%d×]", g.Count)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// humanBytes renders 1.2 KB / 2.3 MB style sizes.
func humanBytes(n int) string {
	const kb, mb, gb = 1 << 10, 1 << 20, 1 << 30
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// MD renders a markdown report suitable for PR comments and issues.
func MD(d *Data) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## ci-brief: %s\n", mdHeader(d))
	for _, w := range d.Info.Warnings {
		fmt.Fprintf(&b, "\n> ⚠️ %s\n", w)
	}
	switch {
	case d.Res.Degraded:
		fmt.Fprintf(&b, "\n**%d error line(s), no repeating groups:**\n\n```text\n%s\n```\n",
			d.Res.ErrorLines, truncate(d.Res.FirstError, 200))
		return b.String()
	case len(d.Res.Groups) == 0:
		fmt.Fprintf(&b, "\nNo errors found in logs.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n**%d errors → %d groups:**\n\n", d.Res.ErrorLines, len(d.Res.Groups))
	primaryIdx := 0
	for i, g := range d.Res.Groups {
		if d.Res.Primary != nil && g == d.Res.Primary {
			primaryIdx = i + 1
		}
		fmt.Fprintf(&b, "%d. **%s** `%s`\n", i+1, groupBadge(g), mdCode(g.Signature))
		if g.FirstFile != "" {
			fmt.Fprintf(&b, "   - first: `%s`\n", g.FirstFile)
		}
	}
	if d.Res.Primary != nil {
		fmt.Fprintf(&b, "\n**Primary error (hypothesis):** group %d — found in %d of %d jobs.\n",
			primaryIdx, len(d.Res.Primary.Jobs), d.Info.TotalJobs)
	}
	return b.String()
}

func mdHeader(d *Data) string {
	switch d.Mode {
	case ModeJob:
		return fmt.Sprintf("job %q %s, %s", d.Info.Title, statusWord(d.Info.Conclusion), logStats(d.Info))
	case ModeFiles:
		return fmt.Sprintf("%d files, %s", d.Info.TotalJobs, logStats(d.Info))
	default:
		return fmt.Sprintf("run #%d %s%s, %s", d.Info.RunNumber, statusWord(d.Info.Conclusion),
			workflowSuffix(d.Info.Workflow, d.Info.Title), logStats(d.Info))
	}
}

func logStats(i logsource.RunInfo) string {
	return fmt.Sprintf("%d logs (%s)", i.LogCount, humanBytes(i.LogBytes))
}

// mdCode escapes backticks inside signatures for inline code spans.
func mdCode(s string) string {
	if !strings.Contains(s, "`") {
		return s
	}
	return strings.ReplaceAll(s, "`", "\\`")
}
