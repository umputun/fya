// Package stream writes Claude-compatible print-mode output.
package stream

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Output format identifiers used by Config.Format. Mirrors the values the user
// can pass to --output-format.
const (
	FormatText       = "text"
	FormatJSON       = "json"
	FormatStreamJSON = "stream-json"
)

// Config configures a Writer. SessionID, when set, is used as a fallback when a
// Final result does not include its own session id.
type Config struct {
	Format                   string
	SessionID                string
	ValidateStructuredOutput func(string) (json.RawMessage, error)
}

// Event is one Claude print-mode stream-json event with a nested message body.
// It is used by the PTY/transcript path to relay assistant and tool-result
// messages in the same broad shape as native `claude -p --output-format stream-json`.
type Event struct {
	Type      string
	SessionID string
	Message   json.RawMessage
}

// Result is the final per-turn metadata.
type Result struct {
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	SessionID      string `json:"session_id,omitempty"`
	NumTurns       int    `json:"num_turns"`
	TerminalReason string `json:"terminal_reason"`
}

// Writer serializes Claude-compatible print-mode events to an io.Writer. Final
// is idempotent — only the first call emits the result, subsequent calls no-op.
type Writer struct {
	cfg    Config
	out    io.Writer
	text   strings.Builder
	closed bool
}

// NewWriter returns a Writer that emits to out using cfg. An empty Format
// defaults to FormatText.
func NewWriter(out io.Writer, cfg Config) *Writer {
	if cfg.Format == "" {
		cfg.Format = FormatText
	}
	return &Writer{out: out, cfg: cfg}
}

// Text records assistant text. In stream-json mode it emits a complete
// assistant message event instead of legacy content_block_delta records because
// current Claude print mode streams message-shaped events.
func (w *Writer) Text(delta string) error {
	if delta == "" {
		return nil
	}
	w.text.WriteString(delta)
	switch w.cfg.Format {
	case FormatStreamJSON:
		return w.writeTextEvent(delta)
	case FormatText, FormatJSON:
		return nil
	default:
		return fmt.Errorf("unsupported output format: %s", w.cfg.Format)
	}
}

// Event emits a Claude-compatible stream-json message event. For text/json
// formats it only accumulates assistant text into the final result.
func (w *Writer) Event(event Event) error {
	if len(event.Message) == 0 {
		return nil
	}
	if event.Type == "assistant" {
		w.text.WriteString(messageText(event.Message))
	}
	if w.cfg.Format != FormatStreamJSON {
		return nil
	}
	var msg any
	if err := json.Unmarshal(event.Message, &msg); err != nil {
		return fmt.Errorf("parse stream message: %w", err)
	}
	obj := map[string]any{"type": event.Type, "message": msg}
	if event.SessionID != "" {
		obj["session_id"] = event.SessionID
	}
	return w.writeJSON(obj)
}

// Final emits the terminal result event. It is idempotent: subsequent calls
// after the first are silently dropped so a defer-based emission cannot collide
// with an explicit final write.
func (w *Writer) Final(result Result) error {
	if w.closed {
		return nil
	}
	w.closed = true
	if result.Subtype == "" {
		result.Subtype = "success"
	}
	if result.TerminalReason == "" {
		result.TerminalReason = "end_turn"
	}
	if result.NumTurns == 0 {
		result.NumTurns = 1
	}
	if result.SessionID == "" {
		result.SessionID = w.cfg.SessionID
	}
	if result.Result == "" {
		result.Result = w.text.String()
	}

	switch w.cfg.Format {
	case FormatText:
		return w.writeText(result.Result)
	case FormatJSON:
		return w.writeJSONResult(result)
	case FormatStreamJSON:
		return w.writeJSON(w.resultObject(result))
	default:
		return fmt.Errorf("unsupported output format: %s", w.cfg.Format)
	}
}

func (w *Writer) writeTextEvent(text string) error {
	return w.writeJSON(map[string]any{
		"type":       "assistant",
		"session_id": w.cfg.SessionID,
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	})
}

func messageText(raw json.RawMessage) string {
	var msg struct {
		Content any `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	return contentText(msg.Content)
}

func contentText(content any) string {
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
			text, _ := block["text"].(string)
			out.WriteString(text)
		}
		return out.String()
	default:
		return ""
	}
}

func (w *Writer) writeText(text string) error {
	if _, err := fmt.Fprint(w.out, text); err != nil {
		return fmt.Errorf("write text result: %w", err)
	}
	if strings.HasSuffix(text, "\n") {
		return nil
	}
	if _, err := fmt.Fprintln(w.out); err != nil {
		return fmt.Errorf("write text result newline: %w", err)
	}
	return nil
}

func (w *Writer) writeJSONResult(result Result) error {
	obj, err := w.jsonResultObject(result)
	if writeErr := w.writeJSON(obj); writeErr != nil {
		return writeErr
	}
	return err
}

func (w *Writer) jsonResultObject(result Result) (map[string]any, error) {
	obj := w.resultObject(result)
	if result.IsError || w.cfg.ValidateStructuredOutput == nil {
		return obj, nil
	}
	raw, err := w.cfg.ValidateStructuredOutput(result.Result)
	if err != nil {
		return w.validationErrorObject(result, err), fmt.Errorf("validate structured output: %w", err)
	}
	obj["structured_output"] = raw
	return obj, nil
}

func (w *Writer) validationErrorObject(result Result, err error) map[string]any {
	return map[string]any{
		"type":            "result",
		"subtype":         "error",
		"is_error":        true,
		"result":          fmt.Sprintf("structured output validation failed: %v", err),
		"session_id":      result.SessionID,
		"num_turns":       result.NumTurns,
		"terminal_reason": "fya_structured_output_invalid",
	}
}

func (w *Writer) resultObject(result Result) map[string]any {
	return map[string]any{
		"type":            "result",
		"subtype":         result.Subtype,
		"is_error":        result.IsError,
		"result":          result.Result,
		"session_id":      result.SessionID,
		"num_turns":       result.NumTurns,
		"terminal_reason": result.TerminalReason,
	}
}

func (w *Writer) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal stream event: %w", err)
	}
	if _, err := fmt.Fprintln(w.out, string(data)); err != nil {
		return fmt.Errorf("write stream event: %w", err)
	}
	return nil
}
