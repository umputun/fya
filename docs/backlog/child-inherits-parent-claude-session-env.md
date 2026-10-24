---
worth: maybe
where: app/ptyrun/driver.go:Config.filteredEnv
added: 2026-10-24
---
# child Claude inherits the parent Claude Code session's variables

`filteredEnv` strips `CLAUDECODE` but passes the rest of a parent Claude Code session's environment to the
child: `CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_CODE_MESSAGING_SOCKET`,
`CLAUDE_CODE_MESSAGING_TOKEN`, `CLAUDE_PID`, `CLAUDE_CODE_ENTRYPOINT` and others. Measured 2026-10-24 on
Claude 2.1.310: fya started from a Claude Code Bash tool typed the prompt and then got no transcript at all,
so the turn ran to `--turn-timeout`. Master and the session-id branch behaved the same, and the same run
with those variables unset returned the answer.

#19 ruled running fya from inside Claude Code out of scope. The open question is whether a wrapper such as
ralphex, started from a Claude Code Bash tool, passes these down too; if so, stripping the parent
session's `CLAUDE_CODE_*` variables in `filteredEnv` is a small fix. Which of them break the child was not
narrowed down.
