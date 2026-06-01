# Gate-Safe Transcript Completion

## Overview
- implement the remaining gate-safe completion design after `28f9119 fix: complete post-tool turns from final assistant text`
- prevent unattended cron gates from holding locks until the 30 minute default timeout when Claude has already written the verdict
- keep the improved default completion semantics, then add an explicit `--gate` profile that only changes timeout defaults

## Context (from discovery)
- files/components involved: `app/transcript/tail.go`, `app/transcript/events.go`, `app/turn/runner.go`, `app/options/options.go`, `README.md`, `ARCHITECTURE.md`
- current baseline: `28f9119` already lets post-tool assistant text complete without a required `stop_reason: "end_turn"`
- current runner idle timing uses parsed transcript events in `app/turn/runner.go`; it does not know when the transcript file changes with ignored metadata or partial trailing JSONL
- current tailer preserves partial trailing lines by not advancing its offset, but it does not report file activity to the runner
- current CLI has `--turn-timeout` defaulting to `30m` and no `--gate` flag or explicit-turn-timeout tracking
- prior-art direction selected in brainstorm: use agentrun-style assistant-text + no-pending-tools + transcript-idle completion; keep hook-first completion as a later design if needed

## Development Approach
- **testing approach**: Regression tests with normal implementation
- complete each task fully before moving to the next
- make small, focused changes
- every code-changing task includes new/updated tests
- all tests must pass before starting next task
- update this plan when scope changes
- maintain backward compatibility unless explicitly rejected

## Code-Quality Rules (HARD — verify against every task before marking complete)

These rules supplement project AGENTS.md/CLAUDE.md and are NOT optional. They are the gate for marking any task complete. If a rule is violated, the task is not done — refactor, re-test, then mark complete.

**Signatures (hard limits):**
- No function or method has 4+ parameters. `ctx context.Context` does not count toward the budget. If you need 4+, use an option struct (e.g., `type fooOpts struct { ... }`).
- No function or method has 4+ return values. Split the function into two single-purpose ones, or return a struct.
- Multiple adjacent same-type parameters (`oldLine, newLine int`) are a swap hazard — review whether they belong on a struct.

**Methods vs standalone helpers (project rule, hard):**
- If a function is called only from methods of a single struct, it MUST be a method on that struct. Calling pattern decides, not field access.
- Standalone helpers are reserved for: (a) constructors and entry points (`Parse...`, `New...`, `Decorate...`), (b) utilities shared by multiple unrelated types or by both standalone functions AND methods, (c) tiny cross-cutting helpers.
- Before adding any standalone helper, mentally walk its callers. If every caller is a method of one type, make the helper a method on that type.

**Visibility (private by default, hard):**
- Lowercase identifiers by default. Only export when an out-of-package caller exists.
- Exception (per AGENTS.md/CLAUDE.md): methods called by other structs in the same package CAN be exported for inter-component API clarity. This is the only exception. It does not extend to types, functions, constants, or variables.
- Before exporting any new identifier, grep for cross-package callers. If none, lowercase it.

**Comments (default: none, hard):**
- Default to writing no comments. Add one only when the WHY is non-obvious (a hidden invariant, a workaround, behavior that would surprise a reader).
- Exported items get godoc comments starting with the name. Unexported items get lowercase non-godoc comments — or no comment at all.
- Never describe WHAT the code does when the code itself is self-evident. Never write multi-paragraph comments on routine helpers.

**Per-task gate (before marking ANY checkbox complete):**
1. Formatter runs clean (`~/.claude/format.sh` or `gofmt -s -w` + `goimports -w`).
2. `golangci-lint run --max-issues-per-linter=0 --max-same-issues=0` reports zero issues.
3. `go test ./... -race` passes.
4. Scan the new code for the four rule classes above. Specifically:
   - Grep new function signatures: `grep -nE '^func.*\(.*,.*,.*,.*\)' app/<path>/*.go` — any hit with 4+ comma-separated params (excluding `ctx`) is a violation. Same for the return-value side.
   - For every new standalone helper, `grep -rn 'helperName(' --include='*.go'` and confirm at least one caller is NOT a method of a single type. If all callers are methods of one type, convert.
   - For every new exported identifier, grep cross-package. If no out-of-package hit, lowercase it.
5. Only after 1–4 pass: mark the task complete.

If a previous task shipped a violation (spotted later by user, reviewer, or yourself): fix it in the next commit BEFORE starting the next task. Do not let violations accumulate.

## Testing Strategy
- add unit coverage for transcript file activity detection, including partial trailing JSONL that grows without producing a complete event
- add runner coverage proving file activity delays idle completion even when semantic completion is otherwise possible
- add options parser coverage for `--gate`, `--gate` plus explicit `--turn-timeout`, and unknown/forwarding behavior
- run `make fmt`, `make lint`, `make test`, and `make build` before considering the plan complete

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with `➕`
- document blockers with `⚠️`
- keep plan in sync with actual work

## Solution Overview
- keep terminal transcript records as immediate completion signals
- keep `28f9119` tracker semantics: post-tool assistant text can complete; `tool_result` alone cannot
- change idle timing to use transcript file stability instead of only parsed-event arrival
- add `--gate` as a timeout profile: if `--turn-timeout` was not explicitly supplied, use `5m`; otherwise respect the explicit timeout
- do not introduce hook-based completion in this plan; it remains a future option if transcript heuristics still fail

## Technical Details
- change the `turn.Tailer` interface from `ReadNew() ([]transcript.Event, error)` to `ReadNew() ([]transcript.Event, bool, error)`, where the boolean reports transcript file activity since the previous read
- implement the activity boolean in `transcript.Tailer` by tracking file size; activity is true on the first read and whenever the current file size differs from the last observed size, including partial trailing lines that do not parse into events
- invariant: any `ReadNew` call returning parsed events must count as activity; defensively reset runner idle timing on `activity || len(events) > 0`
- update `Runner.streamTranscript` to track `lastTranscriptActivityAt`; reset it when the tailer reports activity or parsed events, then pass `time.Since(lastTranscriptActivityAt)` to `Completion.Done`
- update session-exit drain to apply the same semantic completion state after the drain retry window: if the tracker can complete, evaluate `Completion.Done` with an idle duration at least as large as `IdleTimeout` and emit a normal final result; otherwise keep the existing error result
- keep semantic completion state in `transcript.Tracker`; do not let ignored metadata create completion-eligible assistant text
- add `--gate` to consumed wrapper flags in `app/options/options.go`
- keep gate state internal to parsing; do not add `Gate` to exported `options.Config` because the flag only affects timeout defaults
- have `splitter` own explicit wrapper-timeout detection and return an unexported `splitResult{claudeArgs, promptArgs, turnTimeoutExplicit}` plus `error`; make it respect the `--` prompt boundary
- apply the `5m` gate default only when `--gate` is true and no explicit turn timeout was present
- document `--gate` in README and the architecture timeout section

## What Goes Where
- **Implementation Steps**: code, tests, generated mocks, docs, plan lifecycle
- **Post-Completion**: manual/external follow-up only, no checkboxes

## Implementation Steps

### Task 1: Base idle completion on transcript file activity

**Files:**
- Modify: `app/transcript/tail.go`
- Modify: `app/transcript/tail_test.go`
- Modify: `app/turn/runner.go`
- Modify: `app/turn/runner_test.go`
- Regenerate: `app/turn/mocks/tailer.go`

- [x] write tailer tests for complete-line reads, partial trailing-line growth, and no-change polls returning the expected activity boolean
- [x] write runner tests proving transcript activity delays idle completion after assistant text until the file becomes stable
- [x] write runner tests proving session-exit drain succeeds when final assistant text arrives without `result`, `turn_duration`, or `end_turn`
- [x] update `turn.Tailer` and `transcript.Tailer.ReadNew` to return the activity boolean
- [x] update `Runner.streamTranscript` to use transcript activity for idle timing
- [x] update session-exit drain to complete normally after the drain retry window when the tracker has completion-eligible assistant text and no pending tool work
- [x] regenerate affected turn mocks with `go generate ./app/turn`
- [x] run focused tests: `go test ./app/transcript ./app/turn -count=1`
- [x] run quality gate: `make fmt && make lint && make test`

### Task 2: Add `--gate` timeout profile

**Files:**
- Modify: `app/options/options.go`
- Modify: `app/options/options_test.go`
- Modify: `README.md`
- Modify: `ARCHITECTURE.md`

- [x] write parser tests for `--gate` setting `TurnTimeout` to `5m` when `--turn-timeout` is omitted
- [x] write parser tests proving explicit `--turn-timeout=10m` and `--turn-timeout 10m` both win over `--gate`
- [x] write parser tests proving `--turn-timeout` after `--` is prompt text and does not disable the `--gate` 5m default
- [x] add internal gate handling to options parsing without forwarding `--gate` to Claude or exposing `Gate` on `options.Config`
- [x] add explicit `--turn-timeout` detection through splitter-owned parse state, not a naive raw-args scan
- [x] document `--gate` in README and architecture docs
- [x] run focused tests: `go test ./app/options -count=1`
- [x] run quality gate: `make fmt && make lint && make test`

### Task 3: Verify acceptance criteria

**Files:**
- Read/verify: `app/transcript/tail.go`
- Read/verify: `app/turn/runner.go`
- Read/verify: `app/options/options.go`
- Read/verify: `README.md`
- Read/verify: `ARCHITECTURE.md`

- [x] verify terminal records still complete immediately
- [x] verify post-tool assistant text can complete without `end_turn`
- [x] verify `tool_result` alone cannot complete
- [x] verify transcript file activity delays idle completion until stable
- [x] verify session-exit drain can finish from final assistant text when Claude exits before terminal metadata is written
- [x] verify `--gate` only changes the default timeout and explicit `--turn-timeout` still wins
- [x] run final validation: `make fmt && make lint && make test && make build`

### Task 4: [Final] Archive plan

**Files:**
- Move: `docs/plans/20260601-gate-safe-completion.md` to `docs/plans/completed/20260601-gate-safe-completion.md`

- [x] update this plan with any final implementation notes discovered during execution
- [x] move this plan to `docs/plans/completed/`

## Final Implementation Notes

- Archive task required no code changes or generated artifacts.
- Final validation passed with `make fmt && make lint && make test && make build` before moving the completed plan.

## Post-Completion

- Ask the cron gate user to retry with `--gate` after a release or local build is available.
- If transcript heuristics still fail after this plan, revisit hook-first completion using `claude-p`'s Stop-hook pattern.
