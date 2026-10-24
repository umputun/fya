---
worth: maybe
where: app/transcript/path.go:Catalog.projectDir
added: 2026-10-24
---
# forwarded --worktree may move the transcript out of the searched project dir

fya forwards Claude's `--worktree`/`-w`, which makes Claude create a git worktree and run the session
there. If Claude then writes the transcript under the worktree's encoded path, both transcript lookups miss
it: `Select` and `SelectSession` only look in the project dir for `--cwd`, so the turn polls until
`--turn-timeout` with no output. Unverified, needs one live run with `--worktree`.

Found while binding fresh runs to a generated `--session-id` (issue #27); `--worktree` runs were left on the
prompt search for this reason. If the relocation is confirmed, the options are to stop forwarding the flag
or to resolve the worktree path the way Claude does.
