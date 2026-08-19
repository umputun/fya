# Scripts

## `compare-claude-p.sh`

Optional parity check for comparing real `claude -p` stream-json output with `fya`.

Dry-run mode validates the script's local JSONL parsing without calling Claude, covering both the message-shaped assistant events current Claude Code and fya emit and the legacy `content_block_delta` records:

```bash
scripts/compare-claude-p.sh --dry-run
```

Live mode calls Claude twice and consumes quota:

```bash
make build
scripts/compare-claude-p.sh
```

Environment overrides:

- `FYA_BIN` - path to the `fya` binary, default `./.bin/fya`
- `CLAUDE_BIN` - path to Claude Code, default `claude`

The live check validates stream-json syntax, assistant text events, final result fields, and reports whether accumulated text is an exact match. Exact text can differ because the underlying model may produce non-deterministic output.
