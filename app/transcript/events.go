package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event is one parsed Claude Code transcript JSONL record. Text is non-empty only
// for assistant content that should be streamed to the consumer. ProgressText is
// a concise stream-json summary for non-text activity such as tool calls and
// tool errors. Result is true for terminal "result" events.
type Event struct {
	Type          string
	Subtype       string
	Text          string
	ProgressText  string
	SessionID     string
	StopReason    string
	Message       json.RawMessage
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
		Type:       p.stringField(raw, "type"),
		Subtype:    p.stringField(raw, "subtype"),
		SessionID:  p.sessionID(raw),
		StopReason: p.stopReason(raw),
	}
	event.Text = p.extractText(event.Type, raw)
	event.ProgressText = p.extractProgressText(event.Type, raw)
	event.ToolUseIDs = p.toolIDs(raw, "tool_use")
	event.ToolResultIDs = p.toolIDs(raw, "tool_result")
	event.Message = p.streamMessage(event, raw)
	event.Result = event.Type == "result" || (event.Type == "system" && event.Subtype == "turn_duration")
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

func (p *parser) extractProgressText(eventType string, raw map[string]any) string {
	var lines []string
	if p.isAssistant(eventType, raw) {
		p.collectToolUseProgress(raw["content"], &lines)
		if msg, ok := raw["message"].(map[string]any); ok {
			p.collectToolUseProgress(msg["content"], &lines)
		}
	}
	if eventType == "user" {
		p.collectToolResultErrors(raw["content"], &lines)
		if msg, ok := raw["message"].(map[string]any); ok {
			p.collectToolResultErrors(msg["content"], &lines)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func (p *parser) collectToolUseProgress(content any, lines *[]string) {
	items, ok := content.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || p.stringField(block, "type") != "tool_use" {
			continue
		}
		name := p.stringField(block, "name")
		if name == "" {
			name = "tool"
		}
		*lines = append(*lines, "tool: "+name+p.toolInputSummary(block["input"]))
	}
}

func (p *parser) collectToolResultErrors(content any, lines *[]string) {
	items, ok := content.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || p.stringField(block, "type") != "tool_result" {
			continue
		}
		isErr, _ := block["is_error"].(bool)
		if !isErr {
			continue
		}
		text := p.contentSummary(block["content"])
		if text == "" {
			text = "error"
		}
		*lines = append(*lines, "tool error: "+text)
	}
}

func (p *parser) toolInputSummary(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"description", "command", "file_path", "pattern", "path", "url"} {
		if text := p.stringField(m, key); text != "" {
			return " " + p.shorten(text, 180)
		}
	}
	return ""
}

func (p *parser) contentSummary(content any) string {
	switch v := content.(type) {
	case string:
		return p.shorten(v, 180)
	case []any:
		return p.shorten(p.contentText(v), 180)
	default:
		return ""
	}
}

func (*parser) shorten(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
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
	if !ok {
		return nil
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return data
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

// Tracker accumulates per-turn state from a stream of Events: whether assistant
// text has been seen and which tool_use IDs are still awaiting tool_result.
// Apply is the only cross-package entry point; the rest of the state is read
// internally by Completion.Done.
type Tracker struct {
	pending         map[string]struct{}
	sawAssistant    bool
	sawStopReason   bool
	awaitingEndTurn bool
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
	if event.StopReason != "" {
		t.sawStopReason = true
	}
	if event.StopReason == "end_turn" && len(event.ToolUseIDs) == 0 {
		t.awaitingEndTurn = false
	}
	if event.StopReason == "tool_use" || len(event.ToolUseIDs) > 0 {
		t.awaitingEndTurn = true
	}
	for _, id := range event.ToolUseIDs {
		t.pending[id] = struct{}{}
	}
	for _, id := range event.ToolResultIDs {
		delete(t.pending, id)
	}
}

func (t *Tracker) pendingCount() int {
	return len(t.pending)
}

func (t *Tracker) canIdleComplete() bool {
	return t.sawAssistant && (!t.sawStopReason || !t.awaitingEndTurn)
}

func (*parser) stringField(raw map[string]any, name string) string {
	value, _ := raw[name].(string)
	return value
}
