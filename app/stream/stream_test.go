package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamJSONEvents(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatStreamJSON, SessionID: "s1"})

	require.NoError(t, w.Text("hello"))
	require.NoError(t, w.Final(Result{}))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	first := decodeLine(t, lines[0])
	assert.Equal(t, "content_block_delta", first["type"])
	final := decodeLine(t, lines[1])
	assert.Equal(t, "result", final["type"])
	assert.Empty(t, final["result"], "stream-json clears result text to avoid duplication")
	assert.Equal(t, "s1", final["session_id"])
}

func TestTextOutput(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatText})

	require.NoError(t, w.Text("hello"))
	require.NoError(t, w.Final(Result{}))

	assert.Equal(t, "hello\n", out.String())
}

func TestTextOutputKeepsExistingNewline(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatText})

	require.NoError(t, w.Final(Result{Result: "hello\n"}))

	assert.Equal(t, "hello\n", out.String())
}

func TestDefaultOutputFormatIsText(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{})

	require.NoError(t, w.Final(Result{Result: "hello"}))

	assert.Equal(t, "hello\n", out.String())
}

func TestJSONOutput(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatJSON})

	require.NoError(t, w.Text("hello"))
	require.NoError(t, w.Final(Result{TerminalReason: "stop"}))

	event := decodeLine(t, strings.TrimSpace(out.String()))
	assert.Equal(t, "result", event["type"])
	assert.Equal(t, "hello", event["result"])
	assert.Equal(t, "stop", event["terminal_reason"])
}

func TestFinalIsIdempotent(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatText})

	require.NoError(t, w.Final(Result{Result: "one"}))
	require.NoError(t, w.Final(Result{Result: "two"}))

	assert.Equal(t, "one\n", out.String(), "subsequent Final calls are no-ops")
}

func TestUnsupportedOutputFormat(t *testing.T) {
	w := NewWriter(&bytes.Buffer{}, Config{Format: "xml"})

	require.Error(t, w.Text("hello"))
}

func TestUnsupportedOutputFormatOnFinal(t *testing.T) {
	w := NewWriter(&bytes.Buffer{}, Config{Format: "xml"})

	require.Error(t, w.Final(Result{Result: "hello"}))
}

func TestTextOutputWriteError(t *testing.T) {
	w := NewWriter(errWriter{}, Config{Format: FormatText})

	err := w.Final(Result{Result: "hello"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "write text result")
}

func TestTextOutputNewlineWriteError(t *testing.T) {
	w := NewWriter(&errAfterWriter{failAfter: 1}, Config{Format: FormatText})

	err := w.Final(Result{Result: "hello"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "write text result newline")
}

type errWriter struct{}

func (errWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write failed")
}

type errAfterWriter struct {
	writes    int
	failAfter int
}

func (w *errAfterWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("write failed")
	}
	return len(p), nil
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var event map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &event))
	return event
}
