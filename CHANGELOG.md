# Changelog

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
