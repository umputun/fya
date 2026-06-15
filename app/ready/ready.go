// Package ready detects when an interactive Claude PTY is ready for input.
package ready

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

const maxTimeoutOutput = 4000

// DefaultInputReadyMarker is the sequence Claude emits when it switches the
// terminal into bracketed-paste mode (DECSET 2004) as it starts reading input.
// It is the most reliable cross-version signal that the interactive editor is
// attached and will accept a prompt — terminal protocol rather than rendered
// text, so it does not drift between Claude releases like the editor glyphs do.
// Production wiring assigns it to Config.InputReadyMarker; the zero-value Config
// leaves the gate disabled.
const DefaultInputReadyMarker = "\x1b[?2004h"

// ansiEscape matches terminal escape sequences (OSC, CSI, and other ESC-led
// sequences) so they can be stripped before rendered-text matching.
var ansiEscape = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-9;?]*[\x20-\x2f]*[\x40-\x7e]|\x1b[()][0-9A-Za-z]|\x1b[=>]`)

// Source supplies the live PTY output and an exit signal used to decide when
// the wrapped Claude process is ready to receive a typed prompt.
type Source interface {
	Output() string
	Done() <-chan struct{}
}

//go:generate moq -out mocks/source.go -pkg mocks -skip-ensure -fmt goimports . Source

// Config configures a Detector. Zero values fall back to sensible defaults
// inside withDefaults.
type Config struct {
	Timeout         time.Duration
	QuietPeriod     time.Duration
	PollInterval    time.Duration
	Warn            io.Writer
	NonFatalTimeout bool
	Glyphs          []string
	// BlockingPrompts are substrings that, when visible in Output, veto every
	// readiness path. These are dialogs that LOOK stable but require user input
	// that fya cannot supply (trust dialogs, setup prompts). Matching is
	// whitespace- and escape-insensitive (see hasBlockingPrompt) because Claude
	// paints dialog text by positioning the cursor between words rather than
	// emitting literal spaces. Default values cover known Claude Code blockers.
	BlockingPrompts []string
	// InputReadyMarker, when non-empty, is the byte sequence that proves Claude's
	// interactive input reader is attached (production sets it to
	// DefaultInputReadyMarker). While it is set, readiness fires as soon as the
	// marker appears and the weaker quiet-period fallback is disabled, so fya
	// never types into an editor that is painted but not yet reading. Empty keeps
	// the legacy glyph/quiet behavior.
	InputReadyMarker string
}

// Result describes the outcome of a Wait call: whether the source became ready,
// which detection method fired (input-ready / glyph / quiet / timeout /
// process-exit), and the final captured Output snapshot.
type Result struct {
	Ready  bool
	Method string
	Output string
}

// Detector polls a Source until it appears ready for input. The primary signal
// is the configured input-ready marker (Claude's bracketed-paste enable); the
// input-prompt glyphs and the quiet period are fallbacks used only when no
// marker is configured.
type Detector struct {
	cfg Config
}

// NewDetector returns a Detector using cfg with defaults applied for any unset
// numeric fields, glyphs, and blocking prompts.
func NewDetector(cfg Config) *Detector {
	return &Detector{cfg: cfg.withDefaults()}
}

// Wait blocks until src is ready, the deadline elapses, ctx is canceled, or src
// signals exit. When Timeout expires and NonFatalTimeout is true the call writes
// a warning to Warn (if set) and returns Result{Method: "timeout"} with nil
// error unless the captured output contains a blocking prompt.
func (d *Detector) Wait(ctx context.Context, src Source) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("context is nil")
	}
	if src == nil {
		return Result{}, errors.New("ready source is nil")
	}

	deadline := time.NewTimer(d.cfg.Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	lastOutput := src.Output()
	lastChange := time.Now()

	for {
		select {
		case <-src.Done():
			return Result{Ready: false, Method: "process-exit", Output: src.Output()}, errors.New("process exited before ready")
		default:
		}

		if result, ok := d.inspect(src, lastOutput, lastChange); ok {
			return result, nil
		}

		select {
		case <-ctx.Done():
			return Result{}, fmt.Errorf("wait readiness: %w", ctx.Err())
		case <-src.Done():
			return Result{Ready: false, Method: "process-exit", Output: src.Output()}, errors.New("process exited before ready")
		case <-deadline.C:
			return d.timeout(src.Output())
		case <-ticker.C:
			current := src.Output()
			if current != lastOutput {
				lastOutput = current
				lastChange = time.Now()
			}
		}
	}
}

func (d *Detector) inspect(src Source, lastOutput string, lastChange time.Time) (Result, bool) {
	current := src.Output()
	// a visible blocking dialog vetoes EVERY readiness path. If a glyph or the
	// input-ready marker ever coincides with a known blocking dialog (now or in
	// a future Claude UI), the dialog's input requirement takes precedence.
	if d.hasBlockingPrompt(current) {
		return Result{}, false
	}
	// the input-ready marker (bracketed-paste enable) is the most reliable signal
	// that Claude's reader is attached, so it both fires readiness and, while
	// configured, disables the weaker glyph and quiet fallbacks below — which can
	// otherwise promote a painted-but-unread editor to ready and drop the prompt.
	if d.hasInputReady(current) {
		return Result{Ready: true, Method: "input-ready", Output: current}, true
	}
	if d.cfg.InputReadyMarker == "" && d.hasGlyph(current) {
		return Result{Ready: true, Method: "glyph", Output: current}, true
	}
	if d.cfg.InputReadyMarker == "" && current != "" && current == lastOutput && time.Since(lastChange) >= d.cfg.QuietPeriod {
		return Result{Ready: true, Method: "quiet", Output: current}, true
	}
	return Result{}, false
}

func (d *Detector) timeout(output string) (Result, error) {
	result := Result{Ready: false, Method: "timeout", Output: output}
	if d.hasBlockingPrompt(output) {
		return result, errors.New("claude readiness blocked by prompt")
	}
	if d.cfg.NonFatalTimeout {
		d.emitTimeoutWarning(output)
		return result, nil
	}
	return result, errors.New("claude readiness timeout")
}

func (d *Detector) emitTimeoutWarning(output string) {
	if d.cfg.Warn == nil {
		return
	}
	_, _ = fmt.Fprintln(d.cfg.Warn, "warning: Claude readiness timeout; continuing anyway")
	tail := output
	if maxTimeoutOutput > 0 && len(output) > maxTimeoutOutput {
		tail = output[len(output)-maxTimeoutOutput:]
	}
	if tail != "" {
		_, _ = fmt.Fprintf(d.cfg.Warn, "captured Claude terminal output:\n%s\n", tail)
	}
}

// hasInputReady reports whether the configured input-ready marker is present in
// the raw output. The marker is a terminal control sequence, so it is matched
// against the raw stream rather than the normalized visible text.
func (d *Detector) hasInputReady(output string) bool {
	return d.cfg.InputReadyMarker != "" && strings.Contains(output, d.cfg.InputReadyMarker)
}

func (d *Detector) hasGlyph(output string) bool {
	for _, glyph := range d.cfg.Glyphs {
		if strings.Contains(output, glyph) {
			return true
		}
	}
	return false
}

// Blocked reports whether output currently shows a known blocking dialog (such
// as the trust prompt). Callers use it to re-verify, after a post-readiness
// pause, that no dialog has appeared in PTY output arriving since readiness was
// detected — readiness fires on the input-ready marker, which Claude can emit
// before a column-positioned dialog finishes rendering, so typing without this
// re-check could send the prompt into the dialog.
func (d *Detector) Blocked(output string) bool {
	return d.hasBlockingPrompt(output)
}

// hasBlockingPrompt reports whether any blocking-dialog pattern is visible.
// Both the output and the patterns are normalized (escape sequences and all
// whitespace removed) before matching: Claude renders dialog text by moving the
// cursor between words instead of emitting literal spaces, so a plain
// strings.Contains against the raw stream can never match a multi-word phrase.
func (d *Detector) hasBlockingPrompt(output string) bool {
	normalized := d.normalizeForMatch(output)
	for _, blocker := range d.cfg.BlockingPrompts {
		if strings.Contains(normalized, d.normalizeForMatch(blocker)) {
			return true
		}
	}
	return false
}

// normalizeForMatch strips terminal escape sequences and all whitespace from s
// so rendered-text matching survives Claude's per-word cursor positioning.
func (*Detector) normalizeForMatch(s string) string {
	stripped := ansiEscape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, stripped)
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.QuietPeriod <= 0 {
		c.QuietPeriod = 750 * time.Millisecond
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 50 * time.Millisecond
	}
	if len(c.Glyphs) == 0 {
		// only input-editor markers belong here. Status banners can appear
		// before the editor is ready; blocking dialogs are handled separately
		// below so real prompt glyphs still win when Claude is ready. These are
		// a fallback for Claude builds that do not emit InputReadyMarker; the
		// marker is the primary readiness signal in production wiring.
		c.Glyphs = []string{
			"\n> ",
			"\r\n> ",
			"│ > ",
			"│> ",
			"? for shortcuts",
		}
	} else {
		c.Glyphs = slices.Clone(c.Glyphs)
	}
	if c.BlockingPrompts == nil {
		// known Claude Code dialogs that LOOK stable (so a readiness signal could
		// otherwise mis-promote them to ready) but require user input fya cannot
		// supply. Matching is whitespace/escape-insensitive, so these read as the
		// human-visible phrasing regardless of how Claude positions the words.
		c.BlockingPrompts = []string{
			"Do you trust the files in this folder?",         // legacy trust dialog
			"Is this a project you created or one you trust", // current trust dialog heading
			"Yes, I trust this folder",                       // current trust dialog accept option
		}
	} else {
		c.BlockingPrompts = slices.Clone(c.BlockingPrompts)
	}
	return c
}
