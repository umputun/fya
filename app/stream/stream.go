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
	Format    string
	SessionID string
}

// Result is the final per-turn metadata. For stream-json output the Result text
// field is intentionally cleared by Writer.Final so consumers that already
// received text deltas (e.g. Ralphex) do not get duplicate text.
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

// Text emits an assistant text delta. In stream-json mode it writes a
// content_block_delta event; in text/json modes the delta is accumulated into
// the final Result.Result instead.
func (w *Writer) Text(delta string) error {
	if delta == "" {
		return nil
	}
	w.text.WriteString(delta)
	switch w.cfg.Format {
	case FormatStreamJSON:
		return w.writeJSON(map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		})
	case FormatText, FormatJSON:
		return nil
	default:
		return fmt.Errorf("unsupported output format: %s", w.cfg.Format)
	}
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
		return w.writeJSON(w.resultObject(result))
	case FormatStreamJSON:
		streamResult := result
		streamResult.Result = ""
		return w.writeJSON(w.resultObject(streamResult))
	default:
		return fmt.Errorf("unsupported output format: %s", w.cfg.Format)
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
