package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event is one parsed Claude Code transcript JSONL record. Text is non-empty only
// for assistant content that should be streamed to the consumer. Result is true
// for terminal "result" events.
type Event struct {
	Type          string
	Subtype       string
	Text          string
	SessionID     string
	StopReason    string
	Message       json.RawMessage
	ToolUseIDs    []string
	ToolResultIDs []string
	Result        bool
	Timestamp     time.Time
}

// parser converts raw transcript JSONL lines into Event values. It is
// unexported because the only consumer is the in-package Tailer.
type parser struct{}

func (p *parser) parse(line []byte) (Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, fmt.Errorf("parse transcript event: %w", err)
	}
	event := Event{
		Type:       p.stringField(raw, "type"),
		Subtype:    p.stringField(raw, "subtype"),
		SessionID:  p.sessionID(raw),
		StopReason: p.stopReason(raw),
	}
	event.Text = p.extractText(event.Type, raw)
	event.ToolUseIDs = p.toolIDs(raw, "tool_use")
	event.ToolResultIDs = p.toolIDs(raw, "tool_result")
	event.Message = p.streamMessage(event, raw)
	event.Result = event.Type == "result" || (event.Type == "system" && event.Subtype == "turn_duration")
	event.Timestamp = p.timestamp(raw)
	return event, nil
}

// timestamp parses the record's RFC3339 timestamp. Records without one (or
// with an unparseable value) yield the zero time and are never filtered by
// turn start, so metadata records and timestamp-free fixtures keep flowing.
func (p *parser) timestamp(raw map[string]any) time.Time {
	value := p.stringField(raw, "timestamp")
	if value == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// extractText returns assistant content suitable for streaming. Only records whose
// outer type or inner message.role is "assistant" contribute text; user records and
// result records are deliberately empty so they never reach output.Text.
func (p *parser) extractText(eventType string, raw map[string]any) string {
	if !p.isAssistant(eventType, raw) {
		return ""
	}
	if text := p.contentText(raw["content"]); text != "" {
		return text
	}
	if delta, ok := raw["delta"].(map[string]any); ok {
		if text := p.stringField(delta, "text"); text != "" {
			return text
		}
	}
	if msg, ok := raw["message"].(map[string]any); ok {
		return p.contentText(msg["content"])
	}
	return ""
}

func (p *parser) isAssistant(eventType string, raw map[string]any) bool {
	if eventType == "assistant" {
		return true
	}
	msg, ok := raw["message"].(map[string]any)
	return ok && p.stringField(msg, "role") == "assistant"
}

func (p *parser) sessionID(raw map[string]any) string {
	if id := p.stringField(raw, "session_id"); id != "" {
		return id
	}
	return p.stringField(raw, "sessionId")
}

func (p *parser) streamMessage(event Event, raw map[string]any) json.RawMessage {
	if event.Type != "assistant" && (event.Type != "user" || len(event.ToolResultIDs) == 0) {
		return nil
	}
	msg, ok := raw["message"]
	if !ok || !p.messageHasContent(msg) {
		return nil
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return data
}

func (*parser) messageHasContent(msg any) bool {
	m, ok := msg.(map[string]any)
	if !ok {
		return false
	}
	switch content := m["content"].(type) {
	case string:
		return content != ""
	case []any:
		return len(content) > 0
	default:
		return false
	}
}

func (p *parser) stopReason(raw map[string]any) string {
	if reason := p.stringField(raw, "stop_reason"); reason != "" {
		return reason
	}
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return ""
	}
	return p.stringField(msg, "stop_reason")
}

func (p *parser) toolIDs(raw map[string]any, blockType string) []string {
	ids := []string{}
	p.collectToolIDs(raw["content"], blockType, &ids)
	if msg, ok := raw["message"].(map[string]any); ok {
		p.collectToolIDs(msg["content"], blockType, &ids)
	}
	return ids
}

func (p *parser) collectToolIDs(content any, blockType string, ids *[]string) {
	items, ok := content.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || p.stringField(block, "type") != blockType {
			continue
		}
		if id := p.stringField(block, "id"); id != "" {
			*ids = append(*ids, id)
			continue
		}
		if id := p.stringField(block, "tool_use_id"); id != "" {
			*ids = append(*ids, id)
		}
	}
}

func (p *parser) contentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var out strings.Builder
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text := p.stringField(block, "text"); text != "" {
				out.WriteString(text)
			}
		}
		return out.String()
	default:
		return ""
	}
}

// Tracker accumulates per-turn state from a stream of Events: whether
// completion-eligible assistant text has been seen, which tool_use IDs are still
// awaiting tool_result, and whether a tool turn still needs a later assistant
// answer.
// Apply is the only cross-package entry point; the rest of the state is read
// internally by Completion.Done.
type Tracker struct {
	pending           map[string]struct{}
	sawCompletionText bool
	awaitingAnswer    bool
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{pending: map[string]struct{}{}}
}

// Apply folds one Event into the tracker.
func (t *Tracker) Apply(event Event) {
	hasToolUse := event.StopReason == "tool_use" || len(event.ToolUseIDs) > 0
	if hasToolUse {
		t.sawCompletionText = false
		t.awaitingAnswer = true
	}
	for _, id := range event.ToolUseIDs {
		t.pending[id] = struct{}{}
	}
	for _, id := range event.ToolResultIDs {
		delete(t.pending, id)
	}
	if event.Text != "" && !hasToolUse && t.pendingCount() == 0 {
		t.sawCompletionText = true
		t.awaitingAnswer = false
	}
}

func (t *Tracker) pendingCount() int {
	return len(t.pending)
}

func (t *Tracker) canIdleComplete() bool {
	return t.sawCompletionText && !t.awaitingAnswer
}

func (*parser) stringField(raw map[string]any, name string) string {
	value, _ := raw[name].(string)
	return value
}
