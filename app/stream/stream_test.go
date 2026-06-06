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
	assert.Equal(t, "assistant", first["type"])
	assert.Equal(t, "s1", first["session_id"])
	assert.Equal(t, "hello", textFromEvent(t, first))
	final := decodeLine(t, lines[1])
	assert.Equal(t, "result", final["type"])
	assert.Equal(t, "hello", final["result"])
	assert.Equal(t, "s1", final["session_id"])
}

func TestStreamJSONTrimsLineSuffixColon(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatStreamJSON, SessionID: "s1"})

	require.NoError(t, w.Text("Now I'll make the changes:\nkey: value\nCommitting:  "))
	require.NoError(t, w.Final(Result{}))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	first := decodeLine(t, lines[0])
	assert.Equal(t, "Now I'll make the changes\nkey: value\nCommitting", textFromEvent(t, first))
	final := decodeLine(t, lines[1])
	assert.Equal(t, "Now I'll make the changes\nkey: value\nCommitting", final["result"])
}

func TestStreamJSONEventTrimsLineSuffixColon(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatStreamJSON})

	require.NoError(t, w.Event(Event{
		Type:      "assistant",
		SessionID: "s2",
		Message:   json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Hg:\nkey: value"}]}`),
	}))
	require.NoError(t, w.Final(Result{}))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	first := decodeLine(t, lines[0])
	assert.Equal(t, "Hg\nkey: value", textFromEvent(t, first))
	final := decodeLine(t, lines[1])
	assert.Equal(t, "Hg\nkey: value", final["result"])
}

func TestStreamJSONEventPassthrough(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{Format: FormatStreamJSON})

	require.NoError(t, w.Event(Event{
		Type:      "assistant",
		SessionID: "s2",
		Message:   json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"hi"}]}`),
	}))
	require.NoError(t, w.Final(Result{}))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	first := decodeLine(t, lines[0])
	assert.Equal(t, "assistant", first["type"])
	assert.Equal(t, "s2", first["session_id"])
	assert.Equal(t, "hi", textFromEvent(t, first))
	final := decodeLine(t, lines[1])
	assert.Equal(t, "hi", final["result"])
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
	assert.NotContains(t, event, "structured_output")
}

func TestJSONOutputWithStructuredOutput(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{
		Format: FormatJSON,
		ValidateStructuredOutput: func(text string) (json.RawMessage, error) {
			assert.JSONEq(t, `{"summary":"done"}`, text)
			return json.RawMessage(text), nil
		},
	})

	require.NoError(t, w.Text(`{"summary":"done"}`))
	require.NoError(t, w.Final(Result{TerminalReason: "stop"}))

	event := decodeLine(t, strings.TrimSpace(out.String()))
	assert.Equal(t, "result", event["type"])
	result, ok := event["result"].(string)
	require.True(t, ok)
	assert.JSONEq(t, `{"summary":"done"}`, result)
	structured, ok := event["structured_output"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "done", structured["summary"])
	assert.Equal(t, "stop", event["terminal_reason"])
}

func TestJSONStructuredOutputValidatesFinalTextSource(t *testing.T) {
	var out bytes.Buffer
	var got string
	w := NewWriter(&out, Config{
		Format: FormatJSON,
		ValidateStructuredOutput: func(text string) (json.RawMessage, error) {
			got = text
			return json.RawMessage(text), nil
		},
	})

	require.NoError(t, w.Text("I'll check."))
	require.NoError(t, w.Text(`{"summary":"done"}`))
	require.NoError(t, w.Final(Result{FinalText: `{"summary":"done"}`, HasFinalText: true}))

	assert.JSONEq(t, `{"summary":"done"}`, got)
	event := decodeLine(t, strings.TrimSpace(out.String()))
	assert.Equal(t, `I'll check.{"summary":"done"}`, event["result"])
	structured, ok := event["structured_output"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "done", structured["summary"])
}

func TestJSONStructuredOutputValidationFailure(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, Config{
		Format: FormatJSON,
		ValidateStructuredOutput: func(string) (json.RawMessage, error) {
			return nil, errors.New("not schema-valid")
		},
	})

	err := w.Final(Result{Result: "not json", SessionID: "s3"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate structured output")
	event := decodeLine(t, strings.TrimSpace(out.String()))
	assert.Equal(t, "result", event["type"])
	assert.Equal(t, "error", event["subtype"])
	assert.Equal(t, true, event["is_error"])
	result, ok := event["result"].(string)
	require.True(t, ok)
	assert.Contains(t, result, "not schema-valid")
	assert.Equal(t, "s3", event["session_id"])
	assert.Equal(t, "fya_structured_output_invalid", event["terminal_reason"])
	assert.NotContains(t, event, "structured_output")
}

func TestJSONStructuredOutputSkipsExistingErrorResult(t *testing.T) {
	var out bytes.Buffer
	called := false
	w := NewWriter(&out, Config{
		Format: FormatJSON,
		ValidateStructuredOutput: func(string) (json.RawMessage, error) {
			called = true
			return nil, errors.New("should not run")
		},
	})

	err := w.Final(Result{Subtype: "error", IsError: true, Result: "turn failed", TerminalReason: "fya_turn_timeout"})

	require.NoError(t, err)
	assert.False(t, called)
	event := decodeLine(t, strings.TrimSpace(out.String()))
	assert.Equal(t, "error", event["subtype"])
	assert.Equal(t, true, event["is_error"])
	assert.Equal(t, "turn failed", event["result"])
	assert.Equal(t, "fya_turn_timeout", event["terminal_reason"])
	assert.NotContains(t, event, "structured_output")
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

func textFromEvent(t *testing.T, event map[string]any) string {
	t.Helper()
	msg, ok := event["message"].(map[string]any)
	require.True(t, ok)
	content, ok := msg["content"].([]any)
	require.True(t, ok)
	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	text, ok := block["text"].(string)
	require.True(t, ok)
	return text
}
