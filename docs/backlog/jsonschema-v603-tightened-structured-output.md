---
worth: no
where: app/schemaoutput/schemaoutput.go:30
added: 2026-08-09
---
# jsonschema v6.0.3 changed what structured output validates

Recorded so the next review does not re-derive it. The v6.0.2 → v6.0.3 bump (PR #24, merged) is not
strictly behavior-preserving. Measured with a probe mirroring `compile()`'s exact path — `DefaultDraft(Draft7)`
plus `json.Decoder.UseNumber()` on both schema and instance — four deltas are reachable from fya:

| schema / output | v6.0.2 | v6.0.3 |
|---|---|---|
| `{"const":"1"}` vs `1` | accept | reject |
| `{"enum":["1",2]}` vs `1` | accept | reject |
| `{"uniqueItems":true}` vs `["1",1]` | reject | accept |
| `{"type":"string","format":"email"}` vs `"\"unclosed@..."` | accept | reject |

All four are the library becoming spec-correct. The three tightenings come from the numeric-type guard added
at `util.go:323` (`if typeOf(v2) != numberType`) and the `HasPrefix`/`HasSuffix` fix at `format.go:321`; each
removes a false-accept where v6.0.2 handed a caller output violating his own schema. The loosening removes a
false-reject. Rejections surface as `is_error` with `terminal_reason: fya_structured_output_invalid`.

Two facts worth keeping, because both contradict a plausible assumption:

- `format` **is** asserted under Draft7 — `objcompiler.go:437` returns true for any draft below 2019, so the
  email fix is live code here, not dead. Relevant if a `format` rejection is ever reported.
- The upstream NaN/Inf `typeOf` change is unreachable: `UseNumber()` means numbers arrive as `json.Number`,
  never `float64`.

Worth fixing: no. There is no fya code to change, and a test asserting `{"const":"1"}` rejects `1` would be
testing the dependency rather than `schemaoutput`. Only the enum case is subtle — the type gate at
`validator.go:118` blocks a homogeneous `{"enum":["1","2"]}`, so only mixed-type enums are affected.
