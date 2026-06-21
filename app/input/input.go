// Package input reads text and stream-json prompts for one fya turn.
package input

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrEmptyPrompt is returned when no prompt text is available on stdin or in
// the positional args.
var ErrEmptyPrompt = errors.New("prompt is required")

// promptNormalizer makes whitespace deliverable through Claude's TUI: it folds
// CRLF and lone CR to LF (a bare CR would read as Enter and submit early) and
// expands each tab to four spaces. A tab cannot be delivered literally — typed
// rune-by-rune Claude's TUI reads it as an autocomplete key, and even inside a
// bracketed paste it wedges the turn — so it is converted to spaces, the only
// whitespace besides LF that survives. The same normalized prompt is later
// matched against the Claude transcript, so normalizing once here keeps
// injection and transcript selection consistent.
var promptNormalizer = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", "    ")

// Request describes the prompt source for one turn. ReplayUserMessages controls
// whether stream-json user records are re-emitted on Stdout for visibility. Warn,
// when set, receives a one-line notice listing any undeliverable control
// characters stripped from the resolved prompt.
type Request struct {
	Args               []string
	Stdin              io.Reader
	StdinHasData       bool
	Stdout             io.Writer
	InputFormat        string
	ReplayUserMessages bool
	Warn               io.Writer
}

// Reader resolves a prompt from a Request, picking the correct parsing path
// based on the configured input format.
type Reader struct {
	req Request
}

// NewReader returns a Reader bound to req.
func NewReader(req Request) *Reader {
	return &Reader{req: req}
}

// Read returns the resolved prompt. In stream-json mode exactly one user
// message is accepted; multiple user messages are rejected per the v1 contract.
func (r *Reader) Read() (string, error) {
	switch r.req.InputFormat {
	case "", "text":
		return r.readText()
	case "stream-json":
		return r.readStreamJSON()
	default:
		return "", fmt.Errorf("unsupported input format: %s", r.req.InputFormat)
	}
}

func (r *Reader) readText() (string, error) {
	prompt := strings.Join(r.req.Args, " ")
	if strings.TrimSpace(prompt) != "" {
		// Positional prompts are finite and must not wait on an attached but open stdin pipe.
		return r.finalize(prompt), nil
	}
	if r.req.StdinHasData {
		data, err := io.ReadAll(r.req.Stdin)
		if err != nil {
			return "", fmt.Errorf("read prompt: %w", err)
		}
		if len(data) > 0 {
			prompt = strings.TrimRight(string(data), "\r\n")
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return "", ErrEmptyPrompt
	}
	return r.finalize(prompt), nil
}

func (r *Reader) readStreamJSON() (string, error) {
	if !r.req.StdinHasData {
		return "", errors.New("stream-json input requires stdin")
	}
	data, err := io.ReadAll(r.req.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stream-json input: %w", err)
	}
	userPrompt, rawUserLine, err := newStreamJSONParser(string(data)).extractSingleUserPrompt()
	if err != nil {
		return "", err
	}
	if r.req.ReplayUserMessages {
		if r.req.Stdout == nil {
			return "", errors.New("replay user messages requires stdout")
		}
		if _, err := fmt.Fprintln(r.req.Stdout, rawUserLine); err != nil {
			return "", fmt.Errorf("replay user message: %w", err)
		}
	}
	return r.finalize(userPrompt), nil
}

// finalize normalizes newlines then strips control characters Claude's TUI
// cannot accept as literal text, warning once when any are dropped. Running it at
// the single input boundary keeps the injected prompt identical to the one later
// matched against the transcript.
func (r *Reader) finalize(prompt string) string {
	cleaned, total, distinct := stripUndeliverable(promptNormalizer.Replace(prompt))
	if total > 0 && r.req.Warn != nil {
		codes := make([]string, len(distinct))
		for i, c := range distinct {
			codes[i] = fmt.Sprintf("0x%02x", c)
		}
		fmt.Fprintf(r.req.Warn, "warning: removed %d undeliverable control character(s) from prompt: %s\n",
			total, strings.Join(codes, " "))
	}
	return cleaned
}

// isUndeliverable reports whether r is a control character Claude's interactive
// TUI cannot receive as literal prompt text. Typed rune-by-rune such bytes are
// read as control keys (ESC clears the input line) and silently wedge the turn;
// even inside a bracketed paste, ESC and DEL corrupt the input. LF is the only
// control character kept — CR is already folded to LF and tabs expanded to spaces
// by promptNormalizer before this runs.
func isUndeliverable(r rune) bool {
	return r == 0x7f || (r < 0x20 && r != '\n')
}

// stripUndeliverable removes every undeliverable control rune from s, returning
// the cleaned string, the total number removed, and the distinct runes removed in
// first-seen order for reporting. The common no-control path allocates nothing.
func stripUndeliverable(s string) (cleaned string, total int, distinct []rune) {
	if !strings.ContainsFunc(s, isUndeliverable) {
		return s, 0, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	seen := make(map[rune]bool)
	for _, r := range s {
		if isUndeliverable(r) {
			total++
			if !seen[r] {
				seen[r] = true
				distinct = append(distinct, r)
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), total, distinct
}

type streamJSONParser struct {
	data string
}

func newStreamJSONParser(data string) *streamJSONParser {
	return &streamJSONParser{data: data}
}

func (p *streamJSONParser) extractSingleUserPrompt() (string, string, error) {
	var prompt, rawLine string
	userMessages := 0
	for line := range strings.SplitSeq(p.data, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return "", "", fmt.Errorf("parse stream-json input: %w", err)
		}
		if p.eventType(event) != "user" {
			continue
		}
		userMessages++
		if userMessages > 1 {
			return "", "", errors.New("stream-json input supports exactly one user message in v1")
		}
		text := strings.TrimRight(p.extractText(event), "\r\n")
		if strings.TrimSpace(text) == "" {
			return "", "", ErrEmptyPrompt
		}
		prompt = text
		rawLine = line
	}
	if userMessages == 0 {
		return "", "", ErrEmptyPrompt
	}
	return prompt, rawLine, nil
}

func (p *streamJSONParser) eventType(event map[string]any) string {
	if typ, ok := event["type"].(string); ok {
		return typ
	}
	if msg, ok := event["message"].(map[string]any); ok {
		if role, ok := msg["role"].(string); ok {
			return role
		}
	}
	return ""
}

func (p *streamJSONParser) extractText(event map[string]any) string {
	if msg, ok := event["message"].(map[string]any); ok {
		return p.contentText(msg["content"])
	}
	return p.contentText(event["content"])
}

func (p *streamJSONParser) contentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := p.contentItemText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func (p *streamJSONParser) contentItemText(item any) string {
	switch v := item.(type) {
	case string:
		return v
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return text
		}
		if content, ok := v["content"]; ok {
			return p.contentText(content)
		}
	}
	return ""
}
