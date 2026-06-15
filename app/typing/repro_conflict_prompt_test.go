package typing

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reproConflictPrompt mirrors the shape of a git rebase-conflict-resolution
// prompt that hangs in production. It is multi-line and several hundred chars
// but UNDER 100 words, so it falls below the default --max-wpm-size=100 paste
// threshold. All names below are placeholders.
const reproConflictPrompt = `You are resolving a git rebase conflict left by the daemon.
The rebase of branch feature-branch onto main failed
while applying commit a1b2c3d (add config field and update the
storage layer). Open each conflicted file below, resolve every
conflict marker by keeping both intents where possible, then stage the file:
pkg/service/internal/store/records.go
pkg/service/internal/store/records_test.go
pkg/service/internal/store/query_test.go
When all three are staged, run git rebase --continue and report the result.`

// TestConflictPromptUsesBracketedPaste is a RED reproducer for the production
// hang: with the SHIPPED default config (--max-wpm-size=100), a multi-line
// conflict prompt under 100 words is typed rune-by-rune, so every embedded
// newline is emitted as the fragile ESC+CR ("\x1b\r") sequence — the exact trick
// the bracketed-paste fork was supposed to retire. Claude's TUI intermittently
// reads ESC+CR as a submit, fragmenting the prompt and hanging the turn until the
// 15-minute inactivity watchdog kills it.
//
// This test asserts the DESIRED (fixed) behavior, so it FAILS today and turns
// green once Type() frames every prompt in bracketed paste regardless of size.
func TestConflictPromptUsesBracketedPaste(t *testing.T) {
	words := len(strings.Fields(reproConflictPrompt))
	require.LessOrEqual(t, words, 100,
		"repro requires a sub-threshold prompt so paste mode does NOT engage (got %d words)", words)

	inj := NewInjector(Config{
		WPM:        defaultWPM, // 100, the production default
		Jitter:     -1,         // deterministic (no jitter)
		MaxWPMSize: 100,        // the SHIPPED --max-wpm-size default
		Sleeper:    (&fakeSleeper{}).sleep,
	})

	// Sanity: this prompt is large enough to trip the slow-typing warning we see
	// in the production log ("estimated prompt typing duration is ...").
	est := inj.estimate(reproConflictPrompt)
	t.Logf("estimated typing duration: %s over %d chars (%d words)", est.total, est.characters, words)
	assert.Greater(t, est.total, 30*time.Second, "matches the production slow-typing warning")

	var out bytes.Buffer
	require.NoError(t, inj.Type(t.Context(), &out, reproConflictPrompt))
	got := out.String()

	// DESIRED behavior (fails until typing.go is fixed): the prompt must be framed
	// in bracketed paste so embedded newlines stay literal and never read as submit.
	assert.Contains(t, got, bracketedPasteStart,
		"FIX NEEDED: sub-threshold multi-line prompt must use bracketed paste")
	assert.NotContains(t, got, "\x1b\r",
		"FIX NEEDED: embedded newline must NOT be the fragile ESC+CR (fragments the prompt and hangs the turn); got %d occurrences", strings.Count(got, "\x1b\r"))
}
