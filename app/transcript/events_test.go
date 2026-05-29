package transcript

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserExtractsTextAndTools(t *testing.T) {
	line := []byte(`{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"tool1"},{"type":"tool_result","tool_use_id":"tool1"}]}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Equal(t, "hello", event.Text)
	assert.Equal(t, "s1", event.SessionID)
	assert.Equal(t, []string{"tool1"}, event.ToolUseIDs)
	assert.Equal(t, []string{"tool1"}, event.ToolResultIDs)
}

func TestParserSkipsUserRecord(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"please answer"}]}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Empty(t, event.Text, "user record must not stream as content_block_delta")
	assert.False(t, event.Result)
}

func TestParserResultRecordHasNoText(t *testing.T) {
	line := []byte(`{"type":"result","result":"final answer","sessionId":"s9"}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Empty(t, event.Text, "result records are completion metadata, not streamable text")
	assert.True(t, event.Result)
	assert.Equal(t, "s9", event.SessionID)
}

func TestParserAssistantDeltaText(t *testing.T) {
	line := []byte(`{"type":"assistant","delta":{"text":"streamed chunk"},"message":{"role":"assistant"}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Equal(t, "streamed chunk", event.Text)
}

func TestTrackerAndCompletion(t *testing.T) {
	tracker := NewTracker()
	completion := Completion{IdleTimeout: time.Second}
	tracker.Apply(Event{Text: "thinking", ToolUseIDs: []string{"t1"}})

	assert.Equal(t, 1, tracker.pendingCount())
	assert.False(t, completion.Done(tracker, Event{}, 2*time.Second), "with pending tool, completion must wait")

	tracker.Apply(Event{ToolResultIDs: []string{"t1"}})
	assert.True(t, completion.Done(tracker, Event{}, 2*time.Second), "after idle with no pending tools, completion fires")
	assert.True(t, completion.Done(tracker, Event{Result: true}, 0), "explicit result event always completes")
}
