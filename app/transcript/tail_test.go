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

	events, err := tailer.ReadNew()

	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, large, events[0].Text)
	assert.Equal(t, int64(len(content)), tailer.offset)
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

	events, err := tailer.ReadNew()

	require.NoError(t, err, "partial line at EOF must not cause a parse error")
	require.Len(t, events, 1)
	assert.Equal(t, "hi", events[0].Text)
	assert.Equal(t, int64(len(first)), tailer.offset, "offset must not advance over the partial")

	// finish writing the line and confirm the next poll picks it up.
	suffix := `"text":"world"}]}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, writeErr := f.WriteString(suffix)
	require.NoError(t, writeErr)
	require.NoError(t, f.Close())

	events2, err := tailer.ReadNew()
	require.NoError(t, err)
	require.Len(t, events2, 1)
	assert.Equal(t, "world", events2[0].Text)
}
