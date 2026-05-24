# Changelog

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
