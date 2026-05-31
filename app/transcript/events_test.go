package transcript

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserExtractsTextAndTools(t *testing.T) {
	line := []byte(`{"type":"assistant","sessionId":"s1","message":{"stop_reason":"tool_use","content":[` +
		`{"type":"text","text":"hello"},{"type":"tool_use","id":"tool1"},{"type":"tool_result","tool_use_id":"tool1"}]}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Equal(t, "hello", event.Text)
	assert.Equal(t, "s1", event.SessionID)
	assert.Equal(t, "tool_use", event.StopReason)
	assert.Equal(t, []string{"tool1"}, event.ToolUseIDs)
	assert.Equal(t, []string{"tool1"}, event.ToolResultIDs)
	assert.NotEmpty(t, event.Message)
}

func TestParserToolUseProgressText(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool1","name":"Bash","input":{"description":"Run tests","command":"go test ./..."}}]}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Empty(t, event.Text)
	assert.Equal(t, "tool: Bash Run tests\n", event.ProgressText)
}

func TestParserToolResultErrorProgressText(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tool1","is_error":true,"content":"Exit code 1\nfailed hard"}]}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Empty(t, event.Text)
	assert.Equal(t, "tool error: Exit code 1 failed hard\n", event.ProgressText)
}

func TestParserSkipsInitialUserRecord(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"please answer"}]}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Empty(t, event.Text, "initial user record must not stream as assistant output")
	assert.Empty(t, event.Message, "initial user prompt should not be replayed as print-mode output")
	assert.False(t, event.Result)
}

func TestParserStreamsToolResultUserRecord(t *testing.T) {
	line := []byte(`{"type":"user","session_id":"s2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool1","content":"ok"}]}}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.Equal(t, "s2", event.SessionID)
	assert.Equal(t, []string{"tool1"}, event.ToolResultIDs)
	assert.NotEmpty(t, event.Message)
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

func TestParserTurnDurationRecordCompletesTurn(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"turn_duration","sessionId":"s9"}`)
	p := parser{}

	event, err := p.parse(line)

	require.NoError(t, err)
	assert.True(t, event.Result)
	assert.Equal(t, "turn_duration", event.Subtype)
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
	tracker.Apply(Event{Text: "thinking", ToolUseIDs: []string{"t1"}, StopReason: "tool_use"})

	assert.Equal(t, 1, tracker.pendingCount())
	assert.False(t, completion.Done(tracker, Event{}, 2*time.Second), "with pending tool, completion must wait")

	tracker.Apply(Event{ToolResultIDs: []string{"t1"}})
	assert.False(t, completion.Done(tracker, Event{}, 2*time.Second), "tool result still needs a later assistant end_turn")

	tracker.Apply(Event{Text: "answer", StopReason: "end_turn"})
	assert.True(t, completion.Done(tracker, Event{}, 2*time.Second), "after end_turn and idle, completion fires")
	assert.True(t, completion.Done(tracker, Event{Result: true}, 0), "explicit result event always completes")
}

func TestTrackerToolUseWithEndTurnStillWaitsForFollowup(t *testing.T) {
	tracker := NewTracker()
	completion := Completion{IdleTimeout: time.Second}
	tracker.Apply(Event{ToolUseIDs: []string{"t1"}, StopReason: "end_turn"})
	tracker.Apply(Event{ToolResultIDs: []string{"t1"}})

	assert.False(t, completion.Done(tracker, Event{}, 2*time.Second), "tool use still needs a later assistant answer")

	tracker.Apply(Event{Text: "answer", StopReason: "end_turn"})
	assert.True(t, completion.Done(tracker, Event{}, 2*time.Second), "assistant answer after tool result can complete")
}

func TestTrackerDoesNotCompleteToolTurnWithoutFollowup(t *testing.T) {
	tracker := NewTracker()
	completion := Completion{IdleTimeout: time.Second}
	tracker.Apply(Event{Type: "assistant", ToolUseIDs: []string{"t1"}, StopReason: "end_turn"})
	tracker.Apply(Event{ToolResultIDs: []string{"t1"}})

	assert.False(t, completion.Done(tracker, Event{}, time.Minute), "tool turn without final answer or turn_duration must not finish early")
}

func TestTrackerThinkingOnlyDoesNotIdleComplete(t *testing.T) {
	tracker := NewTracker()
	completion := Completion{IdleTimeout: time.Second}
	tracker.Apply(Event{Type: "assistant", StopReason: "end_turn"})

	assert.False(t, completion.Done(tracker, Event{}, 2*time.Second), "thinking-only assistant events must wait for text or turn_duration")
	assert.True(t, completion.Done(tracker, Event{Type: "system", Subtype: "turn_duration", Result: true}, 0))
}

func TestTrackerLegacyCompletionWithoutStopReason(t *testing.T) {
	tracker := NewTracker()
	completion := Completion{IdleTimeout: time.Second}
	tracker.Apply(Event{Text: "answer"})

	assert.True(t, completion.Done(tracker, Event{}, 2*time.Second), "old records without stop_reason still idle-complete")
}
