# Add JSON Schema Structured Output Envelope

## Overview
- Implement fya compatibility for `claude --print --output-format json --json-schema <schema>` consumers.
- Solve the compatibility blocker where structured-output callers reject fya output because `structured_output` is missing.
- Keep the first version narrow: JSON output only, text input only, no automatic retry, no stream-json schema mode.

## Context (from discovery)
- files/components involved: `app/options/options.go`, `app/main.go`, `app/stream/stream.go`, transcript/turn tests, README/ARCHITECTURE docs.
- related patterns found: fya consumes print/output flags and forwards launch flags; structured-output callers expect top-level `structured_output` from Claude's JSON envelope.
- dependencies identified: add a JSON Schema validator that supports draft-07, likely `github.com/santhosh-tekuri/jsonschema/v6`.
- prior art checked: Ralphex lists wrappers for stream-json compatibility, and the referenced wrappers either pass through native `claude --print` or emit normal `result` envelopes, not interactive `structured_output`.
- constraints: do not replace native Claude in PATH with fya because fya starts a child command named `claude`; keep the real Claude binary available.

## Development Approach
- **testing approach**: Regular, with focused tests before or alongside each code change.
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
- unit tests required for option parsing, schema prompt decoration, schema validation, success envelope output, and validation failure output.
- integration-style tests should use fake runner/output paths only; do not run live Claude unless explicitly requested.
- structured-output compatibility should be tested with an envelope containing top-level `structured_output` that downstream callers can decode.
- run `make fmt`, `make test`, and `make lint` before completion.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with `➕`
- document blockers with `⚠️`
- keep plan in sync with actual work

## Solution Overview
- fya consumes `--json-schema` instead of forwarding it to interactive Claude.
- v1 supports only `--output-format=json` and `--input-format=text` with `--json-schema`.
- fya appends a schema-output instruction to the prompt before typing it into the interactive session. This avoids changing user-provided `--append-system-prompt` semantics in v1.
- the final assistant text from successful turns is parsed as one JSON value, validated against the supplied schema, and emitted as top-level `structured_output` in the JSON result envelope.
- schema validation applies only to successful final results. Existing cancellation, timeout, startup, readiness, transcript, and Claude-exit error results bypass schema validation and keep their original failure reason.
- invalid model JSON or schema mismatch after a successful turn emits a valid JSON error result and exits non-zero. No corrective retry in v1.
- direct native `claude --print` pass-through was rejected because it would bypass fya's PTY/transcript behavior and would not solve transient interactive-session handling.

## Technical Details
- `options.Config` gains `JSONSchema string`.
- `--json-schema` moves from forwarded Claude flags to consumed value flags.
- `Config.validate` rejects `JSONSchema != ""` unless `OutputFormat == "json"` and `InputFormat == "text"`.
- Add a focused `app/schemaoutput` package. It owns schema compilation, prompt instruction construction, and final assistant JSON validation. The package name is scoped to this specific feature, not a generic helper bucket.
- `schemaoutput.NewValidator(schema string)` returns a stdlib-only validation hook shaped like `func(string) (json.RawMessage, error)`. The third-party validator type stays inside `app/schemaoutput`.
- `schemaoutput.Instruction(schema string)` returns the prompt instruction text. This is a standalone constructor-style helper used by `app/main.go` and tests.
- `run` validates schema before starting the Claude child process, appends the structured-output instruction to the prompt when `JSONSchema` is set, and passes the validation hook to the stream writer.
- Avoid adding a fourth argument to the turn-runner factory. Replace the current factory function parameters with an unexported request struct so stdout, stderr, options, and validation hook travel as one value.
- `stream.Config` gains a stdlib-only validation hook, not a schema string and not a third-party type.
- `Writer.Final` stays as orchestration. Writer-owned helpers should handle success-envelope construction and validation-error envelope construction.
- validation-error output must be valid JSON on stdout with `type:"result"`, `subtype:"error"`, `is_error:true`, no `structured_output`, and stable `terminal_reason:"fya_structured_output_invalid"`. The returned Go error should still make the process exit non-zero.
- `structured_output` should be emitted as a raw JSON value, not as a quoted string. Object schemas are the main target, but fya should not reject non-object JSON values if the supplied schema allows them.
- successful structured-output JSON results should keep the existing `result` field as the raw assistant text and add `structured_output` beside it.
- diagnostics stay on stderr. Stdout remains machine-readable.

## What Goes Where
- **Implementation Steps**: option parsing, schemaoutput package, output envelope, tests, docs, plan lifecycle.
- **Post-Completion**: live Claude smoke checks and downstream integration wiring only, no checkboxes.

## Implementation Steps

### Task 1: Consume and validate `--json-schema`

**Files:**
- Modify: `app/options/options.go`
- Modify: `app/options/options_test.go`

- [x] move `json-schema` from forwarded value flags to consumed value flags
- [x] add `JSONSchema string` to `options.Config` and raw options
- [x] reject `--json-schema` unless `--output-format=json`
- [x] reject `--json-schema` with `--input-format=stream-json` for v1
- [x] write success tests for consumed parsing and not forwarding `--json-schema` to child Claude
- [x] write error/edge tests for invalid output/input format combinations
- [x] run tests: `make test`

### Task 2: Add schemaoutput package and preflight wiring

**Files:**
- Create: `app/schemaoutput/schemaoutput.go`
- Create: `app/schemaoutput/schemaoutput_test.go`
- Modify: `app/main.go`
- Modify: `app/main_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [x] add JSON Schema validator dependency supporting draft-07, preferably `github.com/santhosh-tekuri/jsonschema/v6`
- [x] implement `schemaoutput.NewValidator(schema string) (func(string) (json.RawMessage, error), error)`
- [x] implement `schemaoutput.Instruction(schema string) string`
- [x] validate schema and build the validation hook in `run` before starting the Claude child process
- [x] append the structured-output instruction to the prompt only when `JSONSchema` is set
- [x] write success tests for valid schema setup, valid instance validation, prompt instruction construction, and unchanged prompt when schema mode is absent
- [x] write error/edge tests for invalid schema preflight failure, invalid JSON output, schema mismatch, and schema-allowed non-object JSON values
- [x] run tests: `make test`

### Task 3: Wire structured output through stream JSON results

**Files:**
- Modify: `app/main.go`
- Modify: `app/main_test.go`
- Modify: `app/stream/stream.go`
- Modify: `app/stream/stream_test.go`

- [x] replace the current turn-runner factory parameters with an unexported request struct to avoid a long signature
- [x] pass the schema validation hook into `stream.Config`
- [x] make `Writer.Final` skip structured validation when `result.IsError` is already true
- [x] emit top-level `structured_output` in `--output-format=json` success results when validation succeeds
- [x] emit the specified JSON error result and return an error when final assistant text fails JSON/schema validation
- [x] keep behavior unchanged for text, stream-json, and json output without `--json-schema`
- [x] write success tests for `structured_output` envelope, preserved raw `result`, and unchanged behavior without `--json-schema`
- [x] write error/edge tests for validation failure envelope and existing error-result bypass
- [x] run tests: `make test`

### Task 4: Add structured-output compatibility regression coverage

**Files:**
- Modify: `app/ralphex_compat_test.go` or create a focused compatibility test file under `app/`
- Modify: `README.md` only if the test exposes a documented behavior gap

- [x] add a test fixture using an object schema with required `summary` and `findings` fields
- [x] write success tests verifying fya JSON output includes `structured_output` as a JSON object, not as a quoted JSON string
- [x] write success tests verifying the Claude extraction shape `{"structured_output": ...}` can be decoded from fya output
- [x] write regression tests verifying Ralphex stream-json compatibility behavior is unchanged when `--json-schema` is absent
- [x] run tests: `make test`

### Task 5: Update docs and operator notes

**Files:**
- Modify: `README.md`
- Modify: `ARCHITECTURE.md`

- [ ] document `--json-schema` support scope: JSON output only, text input only, no retry in v1
- [ ] document top-level `structured_output` behavior and validation failure behavior
- [ ] document that fya must be invoked by absolute path in consumers that also need a real `claude` child binary on PATH
- [ ] document live downstream-integration smoke tests as optional/manual because they consume Claude quota
- [ ] run docs-focused checks: `make test`

### Task 6: Verify acceptance criteria

**Files:**
- Modify: as needed from prior tasks

- [ ] verify `fya --print --output-format=json --json-schema <schema>` emits `structured_output` on valid model JSON
- [ ] verify invalid model JSON returns non-zero and does not emit fake `structured_output`
- [ ] verify existing error results keep their original `terminal_reason` and are not replaced by schema validation errors
- [ ] verify unknown flags and existing forwarded Claude flags still behave as before
- [ ] run formatter: `make fmt`
- [ ] run full test suite: `make test`
- [ ] run linter: `make lint`
- [ ] run build: `make build`

### Task 7: [Final] Archive plan

**Files:**
- Modify: `docs/plans/20260603-structured-output-schema-envelope.md`
- Move: `docs/plans/20260603-structured-output-schema-envelope.md` to `docs/plans/completed/20260603-structured-output-schema-envelope.md`

- [ ] update project guidance if implementation discovers durable patterns worth remembering
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems. No checkboxes.*

- Optional live Claude smoke: run a small schema prompt with real fya and compare envelope shape with native `claude --print --output-format=json --json-schema`.
- Optional downstream smoke: configure a structured-output caller to invoke fya by absolute path while keeping real `claude` on PATH, then run one disposable review or automation job.
- Optional deployment wiring belongs in the consuming project, not in this fya plan.
