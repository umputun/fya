// Package ready detects when an interactive Claude PTY is ready for input.
package ready

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const maxTimeoutOutput = 4000

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
	// BlockingPrompts are substrings that, when visible in Output, veto the
	// quiet-fallback readiness path. These are dialogs that LOOK stable but
	// require user input that fya cannot supply (trust dialogs, setup prompts).
	// Default values cover known Claude Code blockers.
	BlockingPrompts []string
}

// Result describes the outcome of a Wait call: whether the source became ready,
// which detection method fired (glyph / quiet / timeout / process-exit), and the
// final captured Output snapshot.
type Result struct {
	Ready  bool
	Method string
	Output string
}

// Detector polls a Source until it appears ready for input, either by matching
// one of the configured input-prompt glyphs or by the output going quiet.
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
	// a visible blocking dialog vetoes BOTH readiness paths. If a glyph string
	// ever appears as a substring of a known blocking dialog (now or in a
	// future Claude UI), the dialog's input requirement takes precedence over
	// the glyph match.
	if d.hasBlockingPrompt(current) {
		return Result{}, false
	}
	if d.hasGlyph(current) {
		return Result{Ready: true, Method: "glyph", Output: current}, true
	}
	if current != "" && current == lastOutput && time.Since(lastChange) >= d.cfg.QuietPeriod {
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

func (d *Detector) hasGlyph(output string) bool {
	for _, glyph := range d.cfg.Glyphs {
		if strings.Contains(output, glyph) {
			return true
		}
	}
	return false
}

func (d *Detector) hasBlockingPrompt(output string) bool {
	for _, blocker := range d.cfg.BlockingPrompts {
		if strings.Contains(output, blocker) {
			return true
		}
	}
	return false
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
		// below so real prompt glyphs still win when Claude is ready.
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
		// known Claude Code dialogs that LOOK stable (so the quiet-period would
		// otherwise mis-promote them to ready) but require user input fya
		// cannot supply.
		c.BlockingPrompts = []string{
			"Do you trust the files in this folder?",
		}
	} else {
		c.BlockingPrompts = slices.Clone(c.BlockingPrompts)
	}
	return c
}
