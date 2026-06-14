package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTailerReadsLargeLinesAndOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	large := strings.Repeat("x", 128*1024)
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + large + `"}]}}` + "\n"
	content += `{"type":"result","result":"done"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	tailer := NewTailer(path)

	events, activity, err := tailer.ReadNew()

	require.NoError(t, err)
	assert.True(t, activity)
	require.Len(t, events, 2)
	assert.Equal(t, large, events[0].Text)
	assert.Equal(t, int64(len(content)), tailer.offset)

	events, activity, err = tailer.ReadNew()
	require.NoError(t, err)
	assert.False(t, activity)
	assert.Empty(t, events)
}

// when EOF arrives mid-line the tailer must NOT advance Offset past the partial
// bytes nor return a parse error; the next poll picks the line up cleanly once
// it's terminated.
func TestTailerPartialLineSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}` + "\n"
	prefix := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text",`
	require.NoError(t, os.WriteFile(path, []byte(first+prefix), 0o600))
	tailer := NewTailer(path)

	events, activity, err := tailer.ReadNew()

	require.NoError(t, err, "partial line at EOF must not cause a parse error")
	assert.True(t, activity)
	require.Len(t, events, 1)
	assert.Equal(t, "hi", events[0].Text)
	assert.Equal(t, int64(len(first)), tailer.offset, "offset must not advance over the partial")

	appendTranscript(t, path, `"text":"wo`)
	events, activity, err = tailer.ReadNew()
	require.NoError(t, err)
	assert.True(t, activity)
	assert.Empty(t, events)
	assert.Equal(t, int64(len(first)), tailer.offset, "offset must not advance over the growing partial")

	events, activity, err = tailer.ReadNew()
	require.NoError(t, err)
	assert.False(t, activity)
	assert.Empty(t, events)

	appendTranscript(t, path, `rld"}]}}`+"\n")
	events, activity, err = tailer.ReadNew()
	require.NoError(t, err)
	assert.True(t, activity)
	require.Len(t, events, 1)
	assert.Equal(t, "world", events[0].Text)
}

// resume case: the tailer must start at the supplied offset so pre-existing
// history (earlier turns of a resumed session) never reaches the consumer. A
// stale result record in that history would otherwise complete the turn with
// the previous answer.
func TestTailerAtOffsetSkipsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	history := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old answer"}]}}` + "\n"
	history += `{"type":"system","subtype":"turn_duration"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(history), 0o600))
	tailer := NewTailerAt(path, int64(len(history)))

	appendTranscript(t, path, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"new answer"}]}}`+"\n")
	events, activity, err := tailer.ReadNew()

	require.NoError(t, err)
	assert.True(t, activity)
	require.Len(t, events, 1, "history before the offset must not be replayed")
	assert.Equal(t, "new answer", events[0].Text)
	assert.False(t, events[0].Result, "stale terminal records must not leak from history")
}

// a resume offset can land mid-record when the size snapshot races a write; the
// first complete line at the offset is then a fragment and must be skipped, not
// surfaced as a parse error that aborts the turn.
func TestTailerAtOffsetSkipsTornFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	full := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n"
	half := full[:20]
	require.NoError(t, os.WriteFile(path, []byte(half), 0o600))
	tailer := NewTailerAt(path, int64(len(half)))

	appendTranscript(t, path, full[20:]) // writer completes the torn record
	appendTranscript(t, path, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"new"}]}}`+"\n")
	events, activity, err := tailer.ReadNew()

	require.NoError(t, err, "fragment at the resume offset must be skipped, not a parse error")
	assert.True(t, activity)
	require.Len(t, events, 1)
	assert.Equal(t, "new", events[0].Text)
}

// parse errors past the first line at a resume offset must still abort: torn
// tolerance is a one-shot allowance for the snapshot race, not blanket lenience.
func TestTailerAtOffsetStillFailsOnLaterCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	first := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(first), 0o600))
	tailer := NewTailerAt(path, 0)

	appendTranscript(t, path, "not json at all\n")
	_, _, err := tailer.ReadNew()

	require.Error(t, err, "corruption beyond the first line at the offset must surface")
}

func appendTranscript(t *testing.T, path, value string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	_, writeErr := f.WriteString(value)
	require.NoError(t, writeErr)
}
