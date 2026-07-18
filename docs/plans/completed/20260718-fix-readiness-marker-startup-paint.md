# Fix readiness marker firing during Claude Code 2.1.214 startup paint

## Overview

`app/ready` decides when the hidden Claude PTY is ready for a typed prompt. In production it gates on Claude's input-ready marker `ESC[?2004h` (bracketed-paste enable), and while that marker is configured it treats the *first sighting* of the marker as readiness and disables the glyph and quiet-period fallbacks.

On Claude Code 2.1.214 the marker is emitted at Claude's terminal init — verified at byte offset 19 (≈1% into startup, ~450-496 ms after spawn), while the editor is still painting (last paint ~1040-1360 ms). fya then waits the 250 ms `--type-settle` and types into an editor that is not reading yet; the opening characters are dropped, the stored transcript prompt no longer matches what `catalog.Select` searches for, and the turn polls to `--turn-timeout` with no output (`select transcript: context deadline exceeded`). Under ralphex this burns the full turn timeout per iteration.

The fix stops treating marker-presence as proof the editor is reading. Readiness on the marker path now requires the marker **and** the existing quiet-period stability window (`QuietPeriod`, 750 ms) — output non-empty and unchanged long enough that the editor has stopped painting. This is strictly stronger than the current gate — it can only fire *later*, never earlier — so the pre-#15 pure-quiet early-fire risk cannot return.

**Problem it solves:** every turn against Claude Code 2.1.214 hangs to the turn timeout and produces no output.

**Benefit:** fya types only after the editor has actually settled; no dropped characters, no false-hang.

## Context (from discovery)

- Files/components involved:
  - `app/ready/ready.go` — `Detector`, `Config`, `inspect()`, `withDefaults()`, glyph defaults. The core fix lives here.
  - `app/ready/ready_test.go` — existing readiness tests; one asserts the old "marker alone fires readiness" contract and is re-specified.
  - `app/turn/runner.go` — consumer (`ready.Wait` → `settleDelay` → `Blocked` re-check → `inject.Type` → `selectTranscript`). Logic unchanged; one drifted comment (211-213) is corrected. Confirms the downstream mechanism.
  - `app/main.go` — production wiring sets `InputReadyMarker: ready.DefaultInputReadyMarker` and relies on `withDefaults` for `QuietPeriod`. Not modified.
  - `ARCHITECTURE.md` (lines ~160-168, ~346), `CHANGELOG.md` — documentation to update.
- Related patterns found:
  - `inspect()` already receives `lastOutput`/`lastChange` from the `Wait` loop; the quiet-period stability check `current != "" && current == lastOutput && time.Since(lastChange) >= QuietPeriod` already exists for the marker-less fallback. The fix reuses that same window for the marker path — no new knob.
  - `hasInputReady`/`hasGlyph`/`hasBlockingPrompt` are `*Detector` methods; a new stability helper follows that placement.
- Dependencies identified: none new. Pure stdlib (`strings`, `time`).
- Verified against real Claude Code 2.1.214 via a de-nested PTY capture (spawn `claude` with `CLAUDE*`/`CLAUDECODE` stripped, 120×40 pty, trusted cwd so the editor renders):
  - marker at byte offset 19 (~1%), ~450-496 ms after spawn; editor paints until ~1040-1360 ms; all five current default glyphs absent; the 2.1.214 editor prompt is `❯` (U+276F) and the footer is `⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents`.
  - **max intra-startup inter-paint gap after the marker, across 5 runs: 267, 279, 280, 299, 439 ms.** The worst lull (439 ms) is why a 250 ms window is insufficient and the existing 750 ms `QuietPeriod` is the right size (clears 439 ms with ~310 ms margin). This is a fast local machine; laggier transports may lull longer, but the gate stays strictly safer than today regardless.

## Development Approach

- **testing approach**: Regular (code first, then tests) — per user's choice for this fix.
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- maintain backward compatibility (marker-less builds keep the glyph/quiet fallbacks)

## Code-Quality Rules (HARD — verify against every task before marking complete)

These rules supplement project CLAUDE.md and are NOT optional. They are the gate for marking any task complete. If a rule is violated, the task is not done — refactor, re-test, then mark complete.

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
- Exception (per CLAUDE.md): methods called by other structs in the same package CAN be exported for inter-component API clarity. This is the only exception. It does not extend to types, functions, constants, or variables.
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

- **unit tests**: required for every task. All readiness logic is unit-testable against the `Source` mock; no new mocks needed.
- **e2e tests**: none in-repo for this path. The true end-to-end confirmation (real Claude Code 2.1.214, actual char-drop gone) needs a live claude turn and is a manual Post-Completion step, not CI.
- Timing tests use small `QuietPeriod` values (single-digit ms) with the mock source, following the existing `TestDetectorQuietFallbackResetsOnOutputChange` pattern (drive output changes via the `readCh` gate).

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

Split readiness cleanly by whether a marker is configured:

- **Marker configured (production):** ready = `hasInputReady(current)` AND output has been stable for `QuietPeriod`. The `Wait` loop already resets `lastChange` on every output change, so "stable for QuietPeriod" naturally means the editor has stopped painting. The standalone glyph and quiet fallbacks stay disabled on this path.
- **Marker not configured (legacy/tests):** unchanged — glyph, then quiet-for-`QuietPeriod`.

A single `isQuiet` method holds the stability expression once and both paths use it with the same `QuietPeriod`, so there is no duplicated expression. The blocking-prompt veto stays first, so it still overrides both the marker and glyph paths.

**Key design decisions:**
- Reuse the existing `QuietPeriod` (750 ms) for the marker path rather than a new constant. The measurement (worst intra-startup lull 439 ms across 5 runs) shows the window must comfortably exceed ~440 ms; 750 ms fits, and reusing avoids a new Config field, config-selection logic, and an export with no external caller. The ~750 ms "tax" after the editor stops painting is the necessary cost of reliably detecting paint-stop, not waste.
- `--type-settle` is untouched: it remains the post-readiness margin + blocking re-check on top of the gate. `QuietPeriod` gates *readiness*; `--type-settle` adds margin *after* readiness. Complementary, not redundant.
- No new CLI flag (YAGNI): `QuietPeriod` is not currently flag-exposed either; if a laggy environment ever needs a longer window, `--type-settle` already provides runtime post-readiness margin, and exposing `QuietPeriod` is a later, separate change.
- Glyph refresh is defense-in-depth for the marker-less config only (production never consults glyphs); kept minimal and line-anchored.

## Technical Details

**New stability helper** (method on `*Detector`, 3 params — no option-struct trigger):
```go
// isQuiet reports whether output has been non-empty and unchanged for QuietPeriod
// — long enough to count as settled (the editor has stopped painting). Both the
// marker path and the marker-less fallback use it.
func (d *Detector) isQuiet(current, lastOutput string, lastChange time.Time) bool {
    return current != "" && current == lastOutput && time.Since(lastChange) >= d.cfg.QuietPeriod
}
```
`current` and `lastOutput` are adjacent same-type (`string`) params — a swap hazard. The equality is symmetric but the `current != ""` guard is not, and `inspect` uses `current` again for the `Output:` field, so both call sites in `inspect` (marker path and marker-less quiet fallback) pass the arguments in this order: `current, st.lastOutput, st.lastChange`. No param struct.

**`scanState` — the poll-to-poll loop state** (small unexported struct threaded through `inspect`):
```go
type scanState struct {
    lastOutput string
    lastChange time.Time
    markerSeen bool
}
```
`markerSeen` is a **latch**: once the input-ready marker has been seen it stays seen and never resets. The `Wait` loop folds every output read into `markerSeen` (`st.markerSeen = st.markerSeen || d.hasInputReady(current)`) *before* the snapshot is used, so no read can observe the marker without latching it. The latch is required because the early marker (≈1% into startup) can be **evicted from the capped PTY tail buffer** over the longer marker+quiet wait — a fresh read late in startup may no longer contain the marker bytes, so re-checking presence on each read would un-see it. The latch carries the sighting forward. Bundling the three fields into one struct also keeps `inspect` at two params (`current`, `st`), so no signature exceeds the limit.

**Rewritten `inspect`** (2 params: `current string`, `st scanState` — it takes the snapshot the `Wait` loop already read, not a fresh `src.Output()`):
```go
func (d *Detector) inspect(current string, st scanState) (Result, bool) {
    if d.hasBlockingPrompt(current) {
        return Result{}, false
    }
    if d.cfg.InputReadyMarker != "" {
        if st.markerSeen && d.isQuiet(current, st.lastOutput, st.lastChange) {
            return Result{Ready: true, Method: "input-ready", Output: current}, true
        }
        return Result{}, false
    }
    if d.hasGlyph(current) {
        return Result{Ready: true, Method: "glyph", Output: current}, true
    }
    if d.isQuiet(current, st.lastOutput, st.lastChange) {
        return Result{Ready: true, Method: "quiet", Output: current}, true
    }
    return Result{}, false
}
```
`inspect` reads no output of its own — `Wait` reads `src.Output()` once per iteration, folds it into `st.markerSeen`, and passes the snapshot in — so no read path can bypass the marker latch. The marker path gates on the latched `st.markerSeen` (not a fresh `hasInputReady(current)`) AND `isQuiet`; the marker-less path keeps the glyph → quiet order.

**Glyph refresh** in `withDefaults` — append the 2.1.214 editor prompt (line-anchored), keep legacy entries:
```go
c.Glyphs = []string{
    "\n> ", "\r\n> ", "│ > ", "│> ",
    "\n❯ ", "\r\n❯ ", // 2.1.214 editor prompt (U+276F), line-anchored
    "? for shortcuts",
}
```
Note: the new glyphs are line-anchored (`\n❯ `/`\r\n❯ `), matching the existing `\n> ` pattern — a bare `❯ ` would raw-substring-match streamed shell text like `❯ ls`. Glyph matching stays raw-substring, so this is best-effort (escape sequences may interleave the prompt in raw output). It only affects the marker-less path; the blocking-prompt veto still runs first, so a `❯ 1.` inside a trust dialog cannot promote readiness.

**Comment/godoc corrections** (same file, part of Task 1):
- `DefaultInputReadyMarker` godoc: the marker signals bracketed-paste mode is enabled; its *bytes* don't drift across Claude releases, but its *timing relative to editor-attach* does — so presence alone does not prove the reader is attached.
- `Config.InputReadyMarker` godoc: readiness requires the marker AND a stable window (`QuietPeriod`); the glyph and standalone quiet fallbacks apply only when no marker is configured.
- `Detector` type godoc and the `inspect` inline comment: describe marker+quiet, not "fires as soon as the marker appears".
- `Blocked` godoc (`ready.go:202-207`): reword the rationale to "readiness fires on the marker plus a short stable window; a dialog can still finish rendering in a burst after that window began, so the post-settle re-check remains necessary."

## What Goes Where
- **Implementation Steps** (`[ ]`): the `ready.go` fix, the glyph refresh, tests, and doc updates — all in-repo.
- **Post-Completion** (no checkboxes): the live Claude Code 2.1.214 end-to-end confirmation (needs a real claude turn / quota; not CI).

## Implementation Steps

### Task 1: Gate input-ready readiness on marker + quiet-period stable window

**Files:**
- Modify: `app/ready/ready.go`
- Modify: `app/ready/ready_test.go`
- Modify: `app/turn/runner.go` (one comment)

- [x] add the `isQuiet(current, lastOutput string, lastChange time.Time) bool` method on `*Detector` using `d.cfg.QuietPeriod` (see Technical Details)
- [x] rewrite `inspect` so the marker path requires `hasInputReady(current) && isQuiet(...)` and returns not-ready otherwise; route the marker-less glyph/quiet fallbacks through `isQuiet` (the standalone quiet expression is removed in favor of the helper)
- [x] correct the godoc/comments to describe marker+quiet (drop "fires as soon as the marker appears" / "proves the reader is attached"): `DefaultInputReadyMarker`, `Config.InputReadyMarker`, `Detector`, the `inspect` inline comment, and the `Blocked` godoc (`ready.go:202-207`, reworded per Technical Details)
- [x] fix the drifted comment in `app/turn/runner.go:211-213` ("after readiness fired on the input-ready marker" → "after readiness fired (marker + stable window)")
- [x] re-specify `TestDetectorInputReadyMarker`: with a small `QuietPeriod` and stable output + marker present, it fires `input-ready`; the old "marker alone, still-streaming" premise is the bug and is removed
- [x] add a regression test: marker present but output keeps changing (simulated startup paint) → NOT ready, times out (proves early-paint marker no longer fires)
- [x] add a test: marker present, output changes then stabilizes → fires `input-ready` only after `QuietPeriod` of stability (assert elapsed ≥ the window, mirroring `TestDetectorQuietFallbackResetsOnOutputChange`)
- [x] confirm `TestDetectorMarkerGatesFallbacks`, `TestDetectorGlyphGatedByMarker`, and `TestDetectorBlockingPromptVetoesInputReadyWhenColumnPositioned` still pass unchanged
- [x] run `go test ./app/ready/... ./app/turn/... -race` and the Code-Quality per-task gate - must pass before next task

### Task 2: Refresh the glyph fallback list for 2.1.214

**Files:**
- Modify: `app/ready/ready.go`
- Modify: `app/ready/ready_test.go`

- [x] append `"\n❯ "` and `"\r\n❯ "` (2.1.214 editor prompt, U+276F, line-anchored) to the default `Glyphs` in `withDefaults`, keeping the legacy glyphs
- [x] add a test: marker-less config with output containing `\n❯ ` fires `glyph`
- [x] add/confirm a test that a `❯ ` appearing inside a known blocking dialog is still vetoed (veto precedes glyph)
- [x] run `go test ./app/ready/... -race` and the Code-Quality per-task gate - must pass before next task

### Task 3: Verify acceptance criteria
- [x] verify readiness no longer fires on a marker seen during ongoing paint, and does fire once output settles (from Task 1 tests) — `TestDetectorInputReadyMarkerHeldDuringPaint` (timeout, not input-ready) and `TestDetectorInputReadyMarkerFiresAfterOutputStabilizes` (input-ready only after QuietPeriod, elapsed ≥ 55ms) both green
- [x] verify the marker-less fallback path (glyph + quiet) is unchanged — `TestDetectorGlyphReadiness`, `TestDetectorArrowGlyphReadiness`, `TestDetectorQuietFallback*` all pass
- [x] run full test suite: `go test ./... -race` — all packages pass
- [x] run `golangci-lint run --max-issues-per-linter=0 --max-same-issues=0` — zero issues
- [x] verify coverage of `app/ready` did not regress: `go test -cover ./app/ready/...` — 96.3% of statements (well above the ~90% target, maintained/improved)

### Task 4: Update documentation
- [x] update `ARCHITECTURE.md` (readiness section ~160-168 and the summary ~346): the marker path is now marker + the quiet-period stable window; correct "proves the reader is attached" / "does not drift between Claude releases"; keep the marker-less glyph/quiet fallbacks description accurate
- [x] add a `CHANGELOG.md` entry: new `## v0.3.6` section with a `### Bug Fixes` heading, phrased as a follow-up correction to the v0.3.4 marker gating (readiness no longer fires during Claude Code 2.1.214 startup paint; prompt no longer truncated → no false turn-timeout hang). The version number is finalized at release time
- [x] update `README.md` only if the readiness wording there is now inaccurate (currently it only documents `--readiness-timeout` and `--type-settle`, likely no change) — README readiness wording still accurate — no change
- [x] move this plan to `docs/plans/completed/` — (move performed by exec finalization after review phases)

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- Live confirmation against real Claude Code 2.1.214: run `fya --output-format text --turn-timeout 120s "reply with exactly: PONG"` from a plain shell (or the de-nested agterm capture harness) and confirm `PONG` returns promptly with no truncated prompt in the transcript. This consumes a real claude turn/quota, so it is not part of CI.
- The 750 ms `QuietPeriod` clears the worst observed intra-startup lull (439 ms) with margin. If a laggier environment still drops opening characters, the runtime lever is `--type-settle` (post-readiness margin); a longer readiness window would require exposing/raising `QuietPeriod`, a separate change (no flag today by design).

---
Smells pre-check: 1 item annotated before save — `isQuiet` `current, lastOutput` swap-hazard noted at the single call site. No signature/return/method-placement/godoc/export violations. (The earlier `MarkerQuietPeriod` export item is moot — the design now reuses `QuietPeriod` with no new field.)
