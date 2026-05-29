# fya <a href="https://github.com/umputun/fya/actions/workflows/ci.yml"><img src="https://github.com/umputun/fya/actions/workflows/ci.yml/badge.svg" alt="build"></a> <a href="https://coveralls.io/github/umputun/fya?branch=master"><img src="https://coveralls.io/repos/github/umputun/fya/badge.svg?branch=master" alt="Coverage Status"></a> <a href="https://goreportcard.com/report/github.com/umputun/fya"><img src="https://goreportcard.com/badge/github.com/umputun/fya" alt="Go Report Card"></a>

`fya` is a Claude Code print-mode wrapper backed by an interactive PTY session.

It provides practical parity with `claude -p` for callers that rely on print-mode text, JSON, or stream-json output. Internally, it starts interactive `claude` in a hidden PTY, types the prompt into the terminal, tails Claude Code transcript logs, and emits Claude-compatible output.

`fya` mirrors the one-shot `claude -p` contract: one process invocation accepts one prompt, streams one answer, emits one final result, and exits after cleanup.

## Requirements

- Go 1.26 to build from source
- Claude Code installed as `claude`
- Unix/macOS for PTY support

Windows returns an unsupported PTY error in v1.

## Installation

**Homebrew:**

```bash
brew install umputun/apps/fya
```

**Binary releases:** download from [GitHub Releases](https://github.com/umputun/fya/releases) for linux/darwin amd64/arm64.

**From source:**

```bash
git clone https://github.com/umputun/fya.git
cd fya
make build
```

The binary is written to `.bin/fya`.

## Usage

`fya` accepts the print-mode shape expected by tools that invoke Claude Code:

```bash
printf 'say hello\n' | fya --print --output-format=stream-json
```

A positional prompt also works without stdin:

```bash
fya --print "say hello"
```

Supported consumed compatibility flags:

- `-p`, `--print`
- `--output-format=text|json|stream-json`
- `--input-format=text|stream-json`
- `--replay-user-messages`

Wrapper controls:

- `--cwd=PATH` - working directory for the interactive Claude session, default `.`
- `--idle-timeout=DURATION` - transcript idle duration before completion, default `2s`
- `--turn-timeout=DURATION` - maximum wall-clock duration for one turn, default `30m`
- `--typing-wpm=N` - prompt typing speed in words per minute, default `100`
- `--typing-jitter=FLOAT` - per-character delay jitter ratio, default `0.20` (0 disables jitter)
- `--max-wpm-size=N` - paste the prompt in one write instead of typing it when the prompt is longer than `N` words, default `100`. `0` always types rune-by-rune. Pasting avoids the multi-minute typing latency of large prompts; typing keeps shorter prompts arriving as individual keystrokes.
- `--readiness-timeout=DURATION` - maximum wait for Claude input readiness, default `30s`
- `--dbg` - enable fya debug logging. Named `--dbg` so it does not collide with Claude's own `--debug` flag, which is forwarded to Claude.

Recognized Claude launch flags are forwarded to interactive `claude`, including `--dangerously-skip-permissions`, `--verbose`, `--model`, `--effort`, permission/tool flags, MCP/config flags, and related interactive Claude flags. Unknown flags fail fast instead of being forwarded.

## Environment

- `FYA_CLAUDE_DIR` - override Claude's config/transcript root for fya transcript discovery. Defaults to `~/.claude`. This is not forwarded to child Claude.
- `DEBUG` - alias for `--dbg`, enables fya debug logging when set to any non-empty value. This is not forwarded to child Claude.
- `ANTHROPIC_API_KEY` - preserved for the child Claude process when present.
- `CLAUDECODE` - stripped from the child process to avoid nested Claude Code session errors.
- `FYA`, `FYA_*`, and shell `_` - stripped from the child process so wrapper-specific environment and command-path details are not leaked through normal environment inspection.

## Ralphex

`fya` can be used in [Ralphex](https://github.com/umputun/ralphex) as the `claude_command` while keeping the usual Claude-compatible arguments:

```ini
claude_command = /path/to/fya
claude_args = --dangerously-skip-permissions --output-format stream-json --verbose
```

Ralphex passes the prompt on stdin and appends `--print`. `fya` consumes print/output flags itself, forwards interactive Claude launch flags such as `--dangerously-skip-permissions`, `--verbose`, `--model`, and `--effort`, and writes JSONL to stdout.

## Optional Parity Check

The live parity script compares `claude -p` stream-json output with `fya`. Dry-run mode does not call Claude:

```bash
scripts/compare-claude-p.sh --dry-run
```

Live mode consumes Claude quota:

```bash
make build
scripts/compare-claude-p.sh
```

## Architecture

- `app/` - executable composition root
- `scripts/` - optional local validation helpers
- `docs/plans/completed/` - completed implementation plans

See [ARCHITECTURE.md](ARCHITECTURE.md) for the PTY flow, transcript tailing, readiness detection, cleanup, and output contract.

## Known Limitations

- v1 supports Unix/macOS PTYs only. Windows returns an unsupported PTY error.
- Transcript parsing follows the current Claude Code JSONL shapes used by the implementation tests. It is not byte-for-byte parity with every Claude stream event.
- `stream-json` emits text deltas plus one final `result`; it intentionally leaves final `result.result` empty so Ralphex does not append already streamed text a second time.
- Multi-turn `--input-format=stream-json` history is outside the one-shot design. Exactly one user message is accepted.
- Live parity checks are manual because they call Claude and consume quota.

## Development

```bash
make test
make lint
make build
```
