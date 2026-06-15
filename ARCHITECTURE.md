# Architecture

`fya` is a one-shot Claude Code wrapper. It exposes a `claude -p` compatible command surface while driving the interactive Claude Code TUI through a PTY. The caller sees print-mode output on stdout; Claude sees keystrokes in a terminal.

The design is intentionally ephemeral:

- one `fya` invocation
- one prompt
- one interactive `claude` child process
- one transcript file selected and tailed
- one final result event, normally after the prompt is accepted for a turn; if fya's turn timeout fires during startup/readiness/typing/transcript selection, an error final result is emitted before transcript streaming begins
- cleanup of the Claude process group before returning

## High-Level Flow

```text
caller
  |
  | args + stdin prompt
  v
app/main.go
  |
  | parse flags, read prompt, wire dependencies
  v
app/turn.Runner
  |
  | start hidden PTY
  v
interactive claude
  |
  | writes transcript JSONL under ~/.claude/projects
  v
app/transcript.Tailer
  |
  | parsed assistant events
  v
app/stream.Writer
  |
  | text/json/stream-json
  v
stdout
```

Stdout is reserved for caller-visible output. Diagnostics, warnings, stack dumps, and logs must go to stderr/logging so `stream-json` consumers such as Ralphex do not see corrupted JSONL.

## Packages

- `app` is the executable composition root. It owns CLI process wiring, signal handling, stdin/stdout/stderr ownership, and dependency construction.
- `app/options` parses fya-compatible flags, splits consumed wrapper flags from forwarded Claude launch flags, and rejects unknown flags.
- `app/input` resolves the prompt from stdin or positional args. Text input accepts stdin with or without a trailing newline. Stream-json input accepts exactly one user message.
- `app/schemaoutput` compiles JSON Schema text, appends the structured-output prompt instruction, and validates a successful final assistant answer as one JSON value.
- `app/ptyrun` starts a command inside a PTY, captures terminal output into a capped tail buffer, exposes process lifecycle methods, and performs graceful cleanup with hard-kill fallback.
- `app/ready` detects when Claude's interactive editor is ready for typed input from PTY output.
- `app/typing` types the prompt rune-by-rune with configurable WPM and jitter, sends multiline input without early submit, and sends the final Enter automatically.
- `app/transcript` discovers Claude Code transcript JSONL files for the current cwd, selects the fresh transcript for the prompt, tails complete lines, parses assistant/tool/result events, exposes streamable message bodies, and decides idle completion.
- `app/stream` converts transcript text, message events, and completion into Claude-compatible `text`, `json`, or `stream-json` output.
- `app/turn` orchestrates one end-to-end turn using consumer-side interfaces and moq-generated mocks in tests.

## CLI And Argument Ownership

`fya` accepts print-mode flags for compatibility, but it does not run `claude --print`.

Consumed by `fya`:

- `-p`, `--print`
- `--output-format`
- `--input-format`
- `--json-schema`
- `--replay-user-messages`
- `--idle-timeout`
- `--turn-timeout`
- `--gate`
- `--cwd`
- `--typing-wpm`
- `--typing-jitter`
- `--max-wpm-size`
- `--readiness-timeout`
- `--type-settle`
- `--silent`
- `--dbg`
- `-V`, `--version`

Forwarded to interactive Claude:

- model and effort flags
- permission/tool flags
- MCP/config flags
- interactive Claude flags such as `--verbose`, `--debug`, `--resume`, `--tmux`, `--worktree`
- `-v`, which belongs to Claude and must not trigger fya version output

Unknown flags fail fast. Unsupported compatibility flags should not be accepted or documented unless fya actually implements their behavior.

Operators must invoke fya by absolute path when a consumer also needs a real Claude child binary. fya starts the child process by running `claude` from `PATH`, so replacing `claude` with fya or placing a fya shim named `claude` ahead of the real binary will recurse or fail before the interactive PTY flow starts.

## Prompt Input

Text mode:

- stdin wins when stdin has data
- positional args are joined with spaces as fallback
- trailing `\r\n` is trimmed
- a missing trailing newline is fine
- internal `\r\n` and lone `\r` are normalized to `\n` so the resolved prompt carries only LF newlines
- empty or whitespace-only prompts fail with `input.ErrEmptyPrompt`

Stream-json input:

- requires stdin
- parses JSONL events
- extracts exactly one user message
- rejects multiple user messages by design
- normalizes the extracted prompt's internal `\r\n` and lone `\r` to `\n` (the replayed raw event keeps its original bytes)
- optionally replays the accepted raw user event when `--replay-user-messages` is set

Prompt submission is independent of the stdin newline. `app/typing` always sends final Enter (`\r`) after typing the prompt.

## PTY Lifecycle

`app/ptyrun.Driver` starts `claude` through `creack/pty.StartWithSize`. That helper creates the child in a new session and assigns the controlling terminal. fya does not set `Setpgid` itself because that conflicts with the PTY session setup on macOS.

The `Process` owns:

- `cmd` for the child process
- `tty` for PTY input/output
- a tail buffer for captured terminal output
- `waitDone`, `drainDone`, and `exited` channels
- idempotent `Close`
- idempotent `Kill`

Three goroutines are started per process:

- `drain` copies PTY output into the tail buffer
- `wait` waits for child exit, closes the PTY, and closes `exited`
- `watchCancel` kills the process group when the context is canceled

Normal cleanup in `turn.Runner` calls `Close` and then `Wait`. `Close` first writes Ctrl-C (`0x03`) to the PTY, matching the usual interactive exit path, then waits two seconds. If Claude is still alive, it sends `SIGTERM` to the process group and waits one more second. If the process still has not exited, it uses the existing `SIGKILL` process-group fallback. `Wait` errors after cleanup are logged at debug level because a signal exit can still happen during fallback, but the process should be reaped before `Run` returns.

## Privacy And Child Environment

`fya` treats its wrapper mechanics as parent-process implementation details. The child Claude process should receive a normal interactive terminal session, not fya-specific environment, prompt markers, diagnostics, or wrapper paths.

Before starting Claude, `app/ptyrun.Config.filteredEnv` removes environment variables that are either consumed by fya or likely to reveal the wrapper path:

- `FYA`
- `FYA_*`, including `FYA_CLAUDE_DIR`
- `DEBUG`, which fya consumes as an alias for `--dbg`
- `CLAUDECODE`, which also avoids nested Claude Code session errors
- `_`, because shells commonly set it to the command path, such as `.bin/fya`

Normal Claude credentials and user environment are preserved. For example, `ANTHROPIC_API_KEY`, `HOME`, `PATH`, `TERM`, and shell configuration remain available unless the caller removes them before launching fya. `PWD` is the exception: when fya starts Claude in a configured cwd, it sets `PWD` to that cwd so the child process cwd and environment agree.

This is best-effort privacy hygiene, not a sandbox boundary. A local program with permission to inspect the host process tree, parent process, filesystem, shell history, or cwd may still be able to infer how it was launched. fya's contract is narrower: do not leak fya-specific details through normal child environment, prompt text, stdout, stderr, transcript parsing, or terminal input paths.

## Readiness Detection

The hidden PTY means fya must infer when it is safe to type.

Readiness detection checks, in order:

1. The input-ready marker: Claude emits `ESC[?2004h` (bracketed-paste enable, DECSET 2004) when it switches the terminal into raw mode and starts reading input. This is the primary signal in production wiring — it is terminal protocol rather than rendered text, so it does not drift between Claude releases like the editor glyphs do, and it proves the reader is attached before fya types. While the marker is configured it also disables the glyph and quiet fallbacks below, closing a race where the editor is painted (or the output merely goes quiet) before Claude reads, so the typed/pasted prompt is dropped. This race surfaces on slow terminal transports such as a Docker Desktop VM.
2. An editor glyph such as `\n> `, `│ > `, or `? for shortcuts`. A fallback for Claude builds that do not emit the marker; like the quiet period it is disabled while the marker is configured.
3. Otherwise, PTY output is non-empty and unchanged for the quiet period. Disabled while the input-ready marker is configured.

Blocking prompts veto every path. These are dialogs that look stable but require user input fya cannot provide, such as Claude's trust prompt. Matching is whitespace- and escape-insensitive: Claude paints dialog text by positioning the cursor between words rather than emitting literal spaces, so a raw substring match can never catch a multi-word phrase. The output and the patterns are both stripped of escape sequences and whitespace before comparison.

`Bypassing Permissions` is not a blocker. It is a status banner and must not prevent readiness when the prompt glyph is present.

`--type-settle` adds a randomized pause (up to +20%) between readiness and typing — distinct from the injector's post-prompt 150 ms settle delay described under Typing — an extra margin on top of the gate for environments whose terminal I/O lags behind the marker. The randomization keeps the interval from being a constant timing fingerprint; the configured value is the floor. After the pause fya re-checks the latest output for a blocking dialog before typing, so a trust prompt that finishes rendering during the window (the marker can arrive before the column-positioned dialog text) is caught instead of being typed into.

Readiness timeout is non-fatal in production wiring unless the captured output contains a blocking prompt. On a normal timeout, fya writes a warning and the captured Claude terminal tail to stderr, then continues. On a blocking-prompt timeout, fya returns an error instead of typing into the wrong UI. This gives diagnosis without corrupting stdout.

## Typing

`app/typing.Injector` writes the prompt to the PTY as keystrokes:

- default rate is 100 WPM
- delay is calculated per Unicode rune, not per byte
- five characters per word are assumed for WPM conversion
- base per-rune delay is `time.Minute / (WPM * 5)`
- at 100 WPM, the base delay is 120 ms per rune
- CLI default jitter is `0.20`, meaning +/-20% around the base delay
- `--typing-jitter=0` disables jitter and uses the exact base delay
- internal newlines are typed as `ESC` + `CR` so the prompt stays in one Claude message
- final submit is always `CR`

For each rune, the injector writes the rune to the PTY and then sleeps for a jittered delay. The jitter formula is:

```text
spread = baseDelay * jitter
delay = baseDelay + spread * random(-1, +1)
```

The random value is uniform in the half-open range `[-1, +1)`. With the default `--typing-wpm=100` and `--typing-jitter=0.20`, each rune sleeps for roughly 96 ms to 144 ms. Negative calculated delays are clamped to zero, which only matters with very large jitter values.

After the last prompt rune, the injector waits for a 150 ms settle delay and then sends the final submit Enter (`CR`). Internal prompt newlines do not submit the message; they emit `ESC` + `CR`, which Claude treats as multiline insertion.

`--max-wpm-size` is a prompt-length threshold measured in words (whitespace-delimited, via `strings.Fields`). The CLI default is `100`. When the prompt has more words than the threshold, the injector skips per-rune pacing and writes the whole prompt in a single write, like a terminal paste, then a settle delay and final `CR`. The whole body — internal LF newlines included — is wrapped in bracketed-paste markers (`ESC[200~` … `ESC[201~`) so the TUI buffers it as one literal paste and never treats an embedded newline as a submit. Paste mode also skips the typing-duration estimate, so the turn-timeout guard and the slow-typing warning never fire for large pasted prompts. `0` disables pasting and always types rune-by-rune; the `typing.Config` zero value is `0`, so the typing engine is opt-in and only the CLI defaults to pasting. Typing rune-by-rune keeps shorter prompts arriving as individual keystrokes rather than a detectable paste block; pasting trades that for avoiding the multi-minute typing latency of very large prompts.

Before typing, the injector estimates duration as:

```text
runeCount(prompt) * baseDelay + settleDelay
```

If estimated typing time exceeds `--turn-timeout`, it fails before launching the prompt into Claude. If the estimate exceeds the warning threshold, currently 30 seconds, fya writes a warning to stderr.

`--gate` is a wrapper-only profile for unattended gate or cron runs. It does not change completion semantics; it enables a `5m` no-activity stall: the turn is aborted when the transcript produces no new activity for `5m` while it is not completable. The clock is measured from the last transcript write, not from turn start, so a long but continuously-active turn is never killed — only a genuine silent hang trips it. `--turn-timeout` (default `30m`) is unchanged by `--gate` and still bounds the whole invocation as the wall-clock ceiling.

## Transcript Discovery

Claude Code writes JSONL transcripts under:

```text
~/.claude/projects/<encoded-cwd>/*.jsonl
```

`FYA_CLAUDE_DIR` can override the Claude root.

The cwd is encoded by replacing every non-letter/digit rune with `-`. Example:

```text
/Users/me/dev/fya
-> -Users-me-dev-fya
```

Transcript selection:

- lists `.jsonl` files for the cwd project directory
- treats a missing directory as retryable
- prefers candidates modified at or after turn start
- requires the prompt to appear in the file
- checks both raw prompt text and JSON-escaped prompt text
- returns `transcript.ErrNoTranscript` when no matching file exists yet

`turn.Runner.selectTranscript` retries `ErrNoTranscript` until a transcript appears, Claude exits, or the turn context is canceled.

## Transcript Tailing

`transcript.Tailer` opens the selected transcript file on each poll, seeks to the current offset, and reads newline-delimited records.

Offsets advance only past complete newline-terminated lines. A partial trailing line is not consumed, so a poll that catches Claude mid-write can re-read the completed line on the next poll.

The tailer also tracks transcript file-size activity. Activity is true on the first read and whenever the file size changes, including partial trailing JSONL and ignored metadata records that do not become output. The runner resets idle completion timing on parsed events or file activity, so idle completion waits for transcript file stability rather than parsed assistant events alone.

The parser emits `transcript.Event` values with:

- assistant text suitable for text/json output
- streamable `assistant` message bodies and `user` tool-result message bodies for stream-json output
- session id
- tool-use ids
- tool-result ids
- result marker

Initial user prompts and result summaries do not produce assistant text.

## Completion Rules

Completion is true when:

- a transcript `result` event or `system`/`turn_duration` record appears, or
- completion-eligible assistant text has appeared, no tool calls are pending, no tool turn is waiting for a later assistant answer, and transcript output has been idle for `--idle-timeout`

Assistant text is completion-eligible only when it is not part of a tool-use event. This lets fya finish when Claude writes a post-tool final answer but omits `stop_reason: "end_turn"`, without finishing immediately after the `tool_result` alone.

If Claude exits before a result event, fya drains the tailer a few more times to catch final transcript writes that landed near process exit. If drained events contain a terminal record or completion-eligible final assistant text, completion is normal. If not, fya emits an error final result and returns an error.

## Output Contract

`app/stream.Writer` supports:

- `text`: collect assistant text and print it at completion
- `json`: emit one final result object
- `stream-json`: emit Claude-style `assistant`/`user` message events as transcript records arrive, then one final `result` with the accumulated assistant answer

For `stream-json`, message-shaped transcript records are relayed instead of converted into legacy `content_block_delta` events. Assistant text is streamed by default. fya does not synthesize `tool:` text progress; tool use is represented only by the relayed Claude message events.

Example:

```json
{"type":"assistant","session_id":"...","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"hello","session_id":"...","num_turns":1,"terminal_reason":"end_turn"}
```

Structured output is enabled only for `--output-format=json --json-schema=SCHEMA` with text input. `--input-format=stream-json` and `--output-format=stream-json` schema mode are rejected in v1, and fya does not perform corrective retries.

When schema mode is enabled, fya compiles the schema before starting Claude, appends a schema-output instruction to the prompt, and validates only successful final results. A successful result keeps `result` as the raw assistant text and adds top-level `structured_output` as the validated JSON value:

```json
{"type":"result","subtype":"success","is_error":false,"result":"{\"summary\":\"done\"}","structured_output":{"summary":"done"},"session_id":"...","num_turns":1,"terminal_reason":"end_turn"}
```

If the successful final assistant text is empty, not exactly one JSON value, or does not validate against the schema, `app/stream.Writer` writes a valid JSON error result, returns an error so the process exits non-zero, and omits `structured_output`:

```json
{"type":"result","subtype":"error","is_error":true,"result":"structured output validation failed: ...","session_id":"...","num_turns":1,"terminal_reason":"fya_structured_output_invalid"}
```

Existing error results from startup, readiness, timeout, cancellation, transcript selection/tailing, or Claude exit bypass structured-output validation and keep their original `terminal_reason`.

## Signals And Cancellation

`main` installs signal handling per invocation:

- `SIGINT` and `SIGTERM` cancel the parent context
- `SIGQUIT` writes a goroutine stack dump to stderr
- cleanup stops signal delivery and waits for the signal goroutine to exit

Cancellation flows through `turn.Runner` into the PTY process. The PTY driver kills the Claude process group on context cancellation because timeout/cancel paths prioritize not leaking child processes. Normal completed turns use graceful `Close` instead.

When fya's own `--turn-timeout` fires, the returned error and the error final result include the stable marker `FYA_TRANSIENT_TIMEOUT` with `terminal_reason: "fya_turn_timeout"`. The `--gate` no-activity stall surfaces the same `FYA_TRANSIENT_TIMEOUT` marker with `terminal_reason: "fya_no_activity_timeout"`. Orchestrators such as Ralphex can classify either as a transient Claude continuation stall and retry it without matching generic `context deadline exceeded` text.

## Diagnostics

Use stderr for diagnosis:

```bash
printf 'hi' | .bin/fya --print --output-format=stream-json --readiness-timeout=5s \
  > /tmp/fya.out 2> /tmp/fya.err
```

Then inspect:

```bash
cat /tmp/fya.err
jq -c . /tmp/fya.out
```

Useful checks:

- readiness timeout output shows captured Claude TUI tail
- `~/.claude/projects/<encoded-cwd>` shows selected transcript candidates
- `--dbg` enables fya debug logging
- `--turn-timeout` bounds the whole invocation
- `--gate` aborts a turn after `5m` of no transcript activity (transient, retryable)
- `--readiness-timeout` bounds the hidden-TUI readiness wait

## Testing

Tests use `testify` and moq-generated mocks for application interfaces. Small hand-written fakes are used only for trivial standard-library-style collaborators such as writers, sleepers, and deterministic randomness.

Important test areas:

- CLI option splitting and forwarded flag ownership
- stdin and stream-json prompt extraction
- PTY lifecycle, Ctrl-C cleanup, and process-group fallback
- readiness input-ready marker, glyph/quiet fallbacks gated behind it, whitespace/escape-insensitive blocking prompt veto, post-settle blocker re-check, and diagnostics
- typing WPM, jitter, multiline handling, and final Enter
- transcript path encoding, prompt matching, complete-line tailing, and completion
- Ralphex stream-json compatibility
- structured-output JSON envelope compatibility and validation failure behavior
- turn orchestration, timeout, transcript retry, Claude exit drain, and cleanup

Live downstream-integration smoke tests are optional and manual because they call Claude and consume quota. They should configure the downstream consumer to invoke fya by absolute path while leaving the real `claude` child binary on `PATH`.
