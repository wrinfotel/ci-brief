# ci-brief

[![CI](https://github.com/wrinfotel/ci-brief/actions/workflows/ci.yml/badge.svg)](https://github.com/wrinfotel/ci-brief/actions/workflows/ci.yml)

> Turn a failed GitHub Actions run into a short, grouped error report — before you open a single megabyte of log.

```text
Run #107554 failed: Frontend tests — Dashboards: Lazy-load edit panels…
11 jobs, 2 failed of 23, 11 logs, 1.2 MB

43 errors → 4 groups:

1. [10 jobs] error: pathspec <path> did not match any file(s) known to git
2. [31×] TypeError: Cannot read properties of undefined (reading 'map')
   first: packages/api/src/auth.ts:84
3. [6×] npm ERR! code E403
4. [2×] ECONNRESET <ip>

Primary error (hypothesis): group 1 — found in 10 of 11 jobs.
```

When CI fails across 10+ jobs, someone opens megabytes of logs and searches for the first error and its repeats by hand. `ci-brief` downloads the failed jobs' logs, normalizes every line, groups identical signatures and points at the likely root cause — deterministically, with regexes, no AI.

- **Deterministic** — the same log always produces the same groups. No LLM, no API calls beyond GitHub.
- **Cascade heuristic** — the group spanning the most jobs is flagged as the probable primary cause.
- **One binary** — Go, zero runtime dependencies, zero external packages.

## Install

```bash
go install github.com/wrinfotel/ci-brief@latest
```

or grab a binary from [Releases](https://github.com/wrinfotel/ci-brief/releases) (linux/darwin/windows, amd64/arm64).

## Usage

```bash
ci-brief                                  # last failed run of the origin repo
ci-brief --repo owner/name --latest       # last failed run
ci-brief --repo owner/name --run 1842     # a specific run
ci-brief --job 123456789                  # one job (repo from --repo or git remote)
ci-brief log1.txt log2.txt                # local log files instead of the API
ci-brief --repo o/n --latest --format json
ci-brief --repo o/n --latest --md out.md  # also save a markdown report
```

| Flag | Meaning |
|---|---|
| `--repo owner/name` | target repository (default: your `git remote origin`) |
| `--run ID` / `--job ID` | analyze one run / one job |
| `--latest` | pick the latest failed run (default when no run/job given) |
| `--format tty\|md\|json` | colored terminal, PR-comment markdown, or machine JSON |
| `--md FILE` | also write the markdown report to FILE |
| `--token TOKEN` | GitHub token (default: `$GITHUB_TOKEN`) |
| `--min-group N` | minimum lines for a group (default 2) |
| `--top N` | show at most N groups (default 8) |
| `--all-jobs` | analyze all jobs, not only failed ones |

Exit codes: `0` — report produced (or nothing to analyze); `2` — utility error. A failed run is **not** an error: ci-brief describes failures, it doesn't gate them.

## Authentication

<!-- TODO(v0.1): record a 10-second demo gif of the grafana case (11 failed jobs → 5 lines) and put it above the first heading. -->

GitHub requires a token to **download job logs even for public repos** (listing runs and jobs works anonymously, but only 60 requests/hour). Create a [fine-grained PAT](https://github.com/settings/personal-access-tokens) with read access to Actions:

```bash
export GITHUB_TOKEN=github_pat_…   # or pass --token
ci-brief --repo grafana/grafana --latest
```

## How grouping works

1. **Normalize** each line: strip ANSI escapes and log timestamps, replace UUIDs, IPs, numbers and paths with placeholders (`<uuid>`, `<ip>`, `<n>`, `<num>`, `<path>`) so 30 copies of the same error collapse into one signature.
2. **Score** each line for error signals (`error`, `FAIL`, `panic:`, `Cannot`, `ECONNRESET`, …) while filtering out compiler-flag traps (`-Werror` is not an error) and CI noise (`retrying…`, `# comments`).
3. **Group** identical signatures, count hits and distinct jobs, and flag the group with the widest job spread (≥3 hits) as the primary-cause hypothesis.
4. **Mask** secrets before anything else: `ghp_…` tokens, `TOKEN=…` values, long alphanumeric runs near secret keywords never reach the report.

When nothing repeats, ci-brief degrades gracefully: "1 error line, no repeating groups" plus the first error line.

## Non-goals (v0.1)

- No fixes or advice — grouping only, no LLM.
- No monitoring — a one-shot report, not a service.
- No GitLab/Jenkins yet (the `logsource` interface leaves room, planned v0.3+).
- No run-to-run diff yet (possible v0.2: `ci-brief diff run1 run2`).

## Development

```bash
go test ./...        # unit + integration tests (offline)
go build .           # local binary
```

testdata/ contains trimmed real-world-shaped logs (grafana pathspec cascade, jest duplicates, a `-Werror` compiler trap, a single-failure degradation case).

## License

[MIT](LICENSE)
