# Changelog

## v0.4.0 - 2026-08-05

### New Features

- Forward `--system-prompt-file` and `--append-system-prompt-file` to Claude. Claude Code accepts the system prompt either inline or from a file, but the forward allowlist carried only the inline pair, so either file variant failed fast with `unknown flag` before Claude started. The inline form rides argv and is capped by the kernel's per-string `MAX_ARG_STRLEN` of 128 KiB — a large assembled system prompt hits `E2BIG` and never runs — while the file variants have no such ceiling, so under fya there was no way to pass one.

### Bug Fixes

- Require the output to settle before typing when readiness is gated on the input-ready marker, correcting the v0.3.4 gating. v0.3.4 treated the marker (`ESC[?2004h`, bracketed-paste enable) as proof the reader was attached, but on Claude Code 2.1.214 the marker fires during startup paint — roughly 800ms before the editor actually reads input — so fya typed the prompt too early, the opening characters were dropped, the stored transcript prompt no longer matched what the catalog searched for, and the turn polled to the turn timeout with no output. Readiness now requires the marker AND the output staying non-empty and unchanged for the quiet period, confirming the editor has stopped painting before fya types; this can only fire later than the old gate, never earlier, so the pre-marker quiet-only early-fire risk cannot return. The marker-less glyph fallback list is also refreshed for the 2.1.214 editor prompt (`❯`, line-anchored).
- Strip undeliverable control characters from prompts so a raw control byte no longer wedges the turn. Prompt text is typed into Claude's interactive editor rune by rune, where a C0 byte is read as a control key rather than text — a single ESC (`0x1b`) clears the input line, the prompt is never submitted, and the turn hangs until timeout with `select transcript: context canceled`. Measurement against the live editor showed every C0 byte `0x01`-`0x1f` plus DEL `0x7f` wedging, tab failing even inside a bracketed paste, and everything at or above `U+0080` delivering literally. Prompt normalization now expands tab to four spaces alongside the existing CRLF folding, drops the remaining C0 bytes (except LF) and DEL, and warns on stderr naming the dropped code points. Sanitizing at the single input boundary keeps the injected prompt identical to the one later matched against the transcript.

## v0.3.5 - 2026-06-16

### Bug Fixes

- Force synchronous sub-agents in the child Claude by defaulting `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` (set only when the caller has not set it). Claude Code 2.1.x launches `Agent`/`Task` sub-agents in the background by default; under fya's single ephemeral PTY turn the main turn ends while those agents are still detached, and fya tears down the PTY before they finish — so orchestrators relying on parallel sub-agents (for example ralphex's review phase) received empty results. Forcing foreground execution makes the sub-agents complete within the turn, matching the headless `claude -p` behavior fya stands in for.

## v0.3.4 - 2026-06-16

### Bug Fixes

- Gate prompt readiness on Claude's input-ready marker (`ESC[?2004h`, bracketed-paste enable) instead of relying on editor glyphs or the quiet-period heuristic. The glyphs had drifted out of date with current Claude, leaving readiness dependent on the quiet heuristic, which can promote a painted-but-unread editor to ready on slow terminal transports such as a Docker Desktop VM — the prompt is then dropped, no transcript is produced, and the turn hangs until timeout. The marker is terminal protocol, proves the reader is attached, and disables the glyph and quiet fallbacks while configured.
- Detect blocking dialogs (the trust prompt) with whitespace- and escape-insensitive matching. Current Claude positions dialog words with cursor moves rather than literal spaces, so the previous raw substring match could never catch the multi-word phrase; also refresh the trust-dialog phrases. After the `--type-settle` pause fya re-checks for a blocking dialog before typing, catching a trust prompt that finished rendering during the window.

### Improvements

- Add `--type-settle` (default `250ms`) to pause after readiness before typing the prompt, an extra margin on top of the readiness gate for environments whose terminal I/O lags (for example a Docker Desktop VM). The pause is randomized up to +20% so it is not a constant timing fingerprint; `0` disables it.

## v0.3.3 - 2026-06-14

### Bug Fixes

- Use bracketed paste for all multi-line prompts, including short prompts under the paste-mode word threshold.

## v0.3.2 - 2026-06-06

### Bug Fixes

- Trim dangling final colons from stream-json assistant output lines while preserving embedded colons such as `key: value`.

## v0.3.1 - 2026-06-04

### Bug Fixes

- Switch `--gate` from a 5m wall-clock turn cap to a 5m idle no-activity timeout measured from the last transcript write, so a long but actively-working turn is no longer killed and only a genuine silent hang trips it. `--turn-timeout` keeps its 30m default.

## v0.3.0 - 2026-06-03

### New Features

- Add `--json-schema` support for JSON output, with fya-owned schema validation and top-level `structured_output` envelope.

## v0.2.5 - 2026-06-03

### Bug Fixes

- Mark fya turn timeouts as transient so wrappers can retry fya-owned turn-timeout failures without parsing generic context deadline errors.

## v0.2.4 - 2026-06-02

### Bug Fixes

- Use bracketed paste for multiline paste-mode prompts so large Ralphex prompts are not split before transcript matching.
- Make unattended gate/cron completion handle delayed or missing Claude terminal metadata, and add `--gate` with a 5m default turn timeout.

## v0.2.3 - 2026-05-31

### Bug Fixes

- Preserve stream-json delta text so delta-only assistant transcript records emit real text instead of empty assistant events.

## v0.2.2 - 2026-05-31

### Bug Fixes

- Fix stream-json Ralphex compatibility.

## v0.2.1 - 2026-05-30

### Bug Fixes

- Fix fya one-shot completion edge cases: prompt source selection no longer blocks on open stdin, tool-use turns wait for the post-tool `end_turn`, and text output ends with one newline.

## v0.2.0 - 2026-05-29

### New Features

- `--max-wpm-size` flag: paste prompts longer than N words (default 100) in a single write instead of typing them rune-by-rune, removing the multi-minute typing latency on large prompts. `--max-wpm-size=0` keeps rune-by-rune typing.

### Improvements

- Normalize internal CRLF and lone CR to LF when resolving the prompt, so a bare carriage return cannot submit a multiline prompt early.
- Internal cleanups to helper ownership and wrapper plumbing.

## v0.1.1 - 2026-05-24

### Bug Fixes

- Switch Homebrew installation from cask to formula to avoid macOS Gatekeeper quarantine prompts.

## v0.1.0 - 2026-05-24

Initial public release.

### New Features

- PTY-backed `claude --print` compatibility wrapper.
- `text`, `json`, and `stream-json` output modes.
- Claude Code transcript discovery and tailing.
- Ralphex-compatible streamed text deltas and final result events.
- Prompt typing controls for WPM, jitter, readiness timeout, turn timeout, and idle timeout.
- Child environment filtering for fya-private variables.
- Release pipeline for GitHub archives, deb/rpm packages, and Homebrew formula installation.
