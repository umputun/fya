package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event is one parsed Claude Code transcript JSONL record. Text is non-empty only
// for assistant content that should be streamed to the consumer; user records and
// result records do not populate Text. Result is true for terminal "result" events.
type Event struct {
	Type          string
	Text          string
	SessionID     string
	ToolUseIDs    []string
	ToolResultIDs []string
	Result        bool
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
		Type:      p.stringField(raw, "type"),
		SessionID: p.stringField(raw, "sessionId"),
	}
	event.Text = p.extractText(event.Type, raw)
	event.ToolUseIDs = p.toolIDs(raw, "tool_use")
	event.ToolResultIDs = p.toolIDs(raw, "tool_result")
	event.Result = event.Type == "result"
	return event, nil
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

// Tracker accumulates per-turn state from a stream of Events: whether assistant
// text has been seen and which tool_use IDs are still awaiting tool_result.
// Apply is the only cross-package entry point; the rest of the state is read
// internally by Completion.Done.
type Tracker struct {
	pending      map[string]struct{}
	sawAssistant bool
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{pending: map[string]struct{}{}}
}

// Apply folds one Event into the tracker.
func (t *Tracker) Apply(event Event) {
	if event.Text != "" {
		t.sawAssistant = true
	}
	for _, id := range event.ToolUseIDs {
		t.pending[id] = struct{}{}
	}
	for _, id := range event.ToolResultIDs {
		delete(t.pending, id)
	}
}

func (t *Tracker) seenAssistant() bool {
	return t.sawAssistant
}

func (t *Tracker) pendingCount() int {
	return len(t.pending)
}

func (*parser) stringField(raw map[string]any, name string) string {
	value, _ := raw[name].(string)
	return value
}
