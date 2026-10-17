# gohook

A Claude Code `PostToolUse` hook for Go. Every time Claude edits a `.go` file, gohook checks that file's package:

1. **gofmt** — the edited file must be formatted (or pass `-fix` to rewrite it automatically).
2. **Lint** — `golangci-lint run` on the package, using the repo's `.golangci.yml` if there is one. Falls back to `go vet` if golangci-lint isn't installed.
3. **Tests and coverage** — `go test` must pass and package statement coverage must be at least 90%.

If anything fails, gohook exits 2 and prints a short report to stderr. Claude Code feeds that back to Claude, which fixes it before moving on. The coverage report lists the uncovered line ranges and the function they're in, so Claude knows exactly what to test:

```
gohook: checks failed after editing calc/calc.go (package ./calc). Fix these before moving on.

## coverage
Package coverage is 16.7% (1/6 statements); the minimum is 90%. Cover at least 5 more statement(s).
Uncovered code:
  calc.go:7-14 in Sign
```

## Install

```bash
go install github.com/rossedman/gohook@latest   # or: go build -o ~/go/bin/gohook .
```

## Configure Claude Code

Add to `.claude/settings.json` in a repo (or `~/.claude/settings.json` for everything):

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Edit|MultiEdit|Write",
        "hooks": [
          { "type": "command", "command": "gohook -min 90 -skip 'cmd/...'", "timeout": 180 }
        ]
      }
    ]
  }
}
```

The `timeout` (seconds) matters: the default hook timeout is shorter than a cold `go test` plus golangci-lint run on a big package.

## Options

| flag | env | default | |
|---|---|---|---|
| `-min` | `GOHOOK_MIN_COVERAGE` | `90` | Minimum package coverage in percent. `0` disables the coverage check. |
| `-skip` | `GOHOOK_SKIP` | | Comma-separated paths to ignore: `path.Match` patterns (`*_gen.go`) or subtrees (`cmd/...`). |
| `-fix` | `GOHOOK_FIX=1` | off | Run `gofmt -w` instead of reporting formatting. |
| `-timeout` | `GOHOOK_TIMEOUT` | `3m` | Time limit for all checks together. |
| `-max-lines` | | `60` | Cap on report lines per check, to keep Claude's context small. |

Flags override environment variables.

Always skipped: non-Go files, files outside a Go module, `vendor/` and `testdata/`, and generated files (`// Code generated ... DO NOT EDIT.`).

## Behavior notes

- **Scope is the edited file's package**, not the whole module, so checks stay fast. Go's test cache helps when nothing changed.
- **gofmt checks only the edited file**; lint and coverage cover the whole package.
- **Editing a test file** re-runs the package's checks too, so Claude sees coverage go up as it writes tests.
- **Packages with no tests fail** with a message telling Claude to add a `_test.go` file. Use `-skip` for packages you don't want held to the bar (often `cmd/...` and `main` packages).
- **Exit codes**: 0 = clean, 2 = checks failed (shown to Claude), 1 = gohook itself errored (shown to you, doesn't block Claude).
