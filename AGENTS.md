# Development Notes

## Commands

- Build: `make build`
- Test: `make test`
- Format: `make fmt`
- Lint: `make lint`
- Version: `make version`

## Style

- Keep Go code formatted with `gofmt -s`.
- Add focused tests with each code change.
- In tests, use `t.Context()` instead of `context.Background()` or `context.TODO()`.
- Prefer simple package boundaries and avoid premature abstraction.
- Keep packages type-oriented. If helper behavior belongs to a struct or is called only from methods of that struct, make it a method instead of a standalone package function.
- Minimize exported API. Export only cross-package entry points; keep parsers, helpers, and package-internal state unexported.
- Define interfaces on the consumer side. Pass interfaces when useful for testing or substitution, but return concrete types when possible.
- Use function types for single-method dependency seams when that is simpler than an interface and does not need a generated mock.
- Group 4+ function parameters into request/config structs; do not grow long execute/run signatures.
- Prefer request-scoped dependency injection in tests over mutable package globals.
- Generate application interface mocks with `moq` via `//go:generate`, store them in a `mocks` package, and do not hand-edit generated files.
- Use `testify` assertions (`require`/`assert`) for normal test expectations. Keep hand-written fakes limited to trivial standard-library collaborators such as `io.Writer`, clocks, sleepers, or deterministic randomness.
- Use `github.com/jessevdk/go-flags` for CLI parsing. New Claude flags that need to be forwarded to the child `claude` process go into the maps in `app/options/options.go` (`forwardBool`, `forwardValue`, `forwardOptionalValue`, `forwardVariadic`); flags fya consumes itself go into `consumedBool` or `consumedValue`.
- Keep the executable composition root in `app/`.
- Put future implementation packages under `app/<concern>` unless a package must be public.
- Keep long CLI examples in `--flag=value` form.

## Runtime Rules

- Stdout is the machine-readable channel. Do not write banners, logs, stack dumps, warnings, or diagnostics to stdout during normal execution; use stderr/logging.
- Treat fya wrapper details as parent-process private. Do not pass fya-specific env, flags, prompt markers, debug text, or helper paths into the child Claude process. `app/ptyrun` filters `FYA`, `FYA_*`, `DEBUG`, `CLAUDECODE`, and shell `_` from the child environment.
- Normal completed-turn cleanup should look like interactive terminal usage: send Ctrl-C through the PTY first, then fall back to `SIGTERM` and `SIGKILL` process-group cleanup only if Claude does not exit.
- `-v` belongs to Claude and must be forwarded. Use `-V`/`--version` for fya version output.
- Do not document or accept Claude compatibility flags unless fya actually implements their behavior. Prefer removing unsupported flags over pretending they are honored.
- Treat Claude transcript creation as asynchronous. Retry transcript selection on `transcript.ErrNoTranscript`, but abort if the Claude session exits or the turn context is canceled.
- When Claude exits during transcript streaming, drain the transcript tailer briefly before declaring failure; a final result event can land just before process exit.
- On incomplete session exit, emit a final `stream.Result` with `IsError: true` and return an error instead of polling forever.
- Tail JSONL by complete newline-terminated records only; do not advance offsets past partial trailing lines.
- Stream-json compatibility means relaying Claude-shaped message events. Do not synthesize `tool:` assistant text from tool_use/tool_result records; Ralphex treats assistant text as user-visible progress and will log synthetic tool text as noise.
- Prompt matching against transcript files must consider both raw and JSON-escaped prompt forms.
- Readiness detection must not promote stable blocking dialogs to ready. Known trust/permission prompts should veto glyph and quiet-period readiness paths.
- `typing-jitter=0` means no jitter. CLI defaults may set nonzero jitter, but internal defaults must not silently re-enable it.
- Name structures by what they actually do. For example, a capped buffer keeping recent output is a tail buffer, not a ring buffer.

## Workflow

- Run `make lint` and `make test` after Go changes.
- Update active plans in `docs/plans/` as implementation progresses.
- Move completed plans to `docs/plans/completed/`.
- Do not run live Claude parity checks unless explicitly requested, because they use Claude quota.

## PTY Privacy Probe

Use a fake `claude` executable in `/tmp` to inspect what the child process can see through fya without spending Claude quota.

Recommended probe shape:

- create `/tmp/fya-pty-probe/probe.go` as a small Go program that prints argv, pid/ppid, cwd, `tty`, `stty -a`, selected env, process ancestry via `ps`, and the bytes read from stdin
- build it as `/tmp/fya-pty-probe-go/claude`
- run fya with `PATH=/tmp/fya-pty-probe-go:$PATH` so fya starts the probe instead of real Claude
- force a readiness timeout by printing the known blocking marker `Do you trust the files in this folder?` from the probe, then inspect fya stderr and the probe's own report file
- run at least one case from a different cwd, for example `/tmp/fya-probe-cwd`, with the built `.bin/fya` plus `--cwd /tmp/fya-probe-cwd`

Check for expected clean signals:

- child argv should be `["claude"]`
- child stdin/stdout/stderr should be TTYs
- child cwd/PWD should match the requested cwd
- prompt input should be the prompt text plus final newline only
- child env should not include `FYA`, `FYA_*`, `DEBUG`, `CLAUDECODE`, or shell `_`
- stdout should remain machine-readable; on forced failure it should be empty or valid stream output, never logs

Known remaining signals:

- process ancestry can show the parent executable and args, such as `/usr/local/bin/fya ...`; handle this separately from env/PTY hygiene
- inherited `PATH` can reveal fya only if a PATH component itself contains a fya-specific directory name
- filesystem context can reveal fya only if the selected cwd or readable project config files mention it
