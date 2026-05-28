// Package typing injects prompts into a PTY with human-like pacing.
package typing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"time"
	"unicode/utf8"
)

const (
	defaultWPM           = 100
	defaultSettleDelay   = 150 * time.Millisecond
	defaultWarnThreshold = 30 * time.Second
	defaultCharsPerWord  = 5
	newlineWithoutSubmit = "\x1b\r"
	submitEnter          = "\r"
)

// SleepFunc waits for d or until ctx is canceled; the real implementation uses
// time.NewTimer with ctx.Done() select.
type SleepFunc func(ctx context.Context, d time.Duration) error

// JitterFunc returns a uniform random float in [0, 1) used to add variation to
// the per-rune typing delay.
type JitterFunc func() float64

// Config configures an Injector. Zero values get sensible defaults except for
// Jitter, where 0 means "no jitter" and a negative value is treated as 0.
type Config struct {
	WPM           int
	Jitter        float64
	SettleDelay   time.Duration
	WarnThreshold time.Duration
	TurnTimeout   time.Duration
	Warn          io.Writer
	Sleeper       SleepFunc
	Rand          JitterFunc
}

// estimate captures the expected duration of typing a prompt with current settings.
type estimate struct {
	characters int
	perRune    time.Duration
	total      time.Duration
}

// Injector types a prompt into an io.Writer at a configurable typing speed.
type Injector struct {
	cfg Config
}

// NewInjector returns an Injector using the supplied Config; defaults are applied
// for unset numeric fields and unset Sleeper/Rand.
func NewInjector(cfg Config) *Injector {
	return &Injector{cfg: cfg.withDefaults()}
}

func (i *Injector) estimate(prompt string) estimate {
	perRune := i.perRuneDelay()
	chars := utf8.RuneCountInString(prompt)
	total := time.Duration(chars)*perRune + i.cfg.SettleDelay
	return estimate{characters: chars, perRune: perRune, total: total}
}

func (i *Injector) validate(prompt string) error {
	est := i.estimate(prompt)
	if i.cfg.TurnTimeout > 0 && est.total > i.cfg.TurnTimeout {
		return fmt.Errorf("estimated typing duration %s exceeds turn timeout %s", est.total, i.cfg.TurnTimeout)
	}
	if i.cfg.Warn != nil && i.cfg.WarnThreshold > 0 && est.total > i.cfg.WarnThreshold {
		if _, err := fmt.Fprintf(i.cfg.Warn, "warning: estimated prompt typing duration is %s\n", est.total); err != nil {
			return fmt.Errorf("write typing warning: %w", err)
		}
	}
	return nil
}

// Type writes prompt rune-by-rune to w, sleeping for a jittered per-rune delay
// between runes, then a settle delay, then a final submit (carriage return).
// Newlines inside the prompt emit ESC+CR (a multi-line insertion without
// submission) so the entire prompt is delivered in one Claude message.
func (i *Injector) Type(ctx context.Context, w io.Writer, prompt string) error {
	if w == nil {
		return errors.New("typing writer is nil")
	}
	if err := i.validate(prompt); err != nil {
		return err
	}
	for _, r := range prompt {
		if err := i.writeRune(w, r); err != nil {
			return err
		}
		if err := i.cfg.Sleeper(ctx, i.jitteredDelay()); err != nil {
			return fmt.Errorf("typing delay: %w", err)
		}
	}
	if err := i.cfg.Sleeper(ctx, i.cfg.SettleDelay); err != nil {
		return fmt.Errorf("settle delay: %w", err)
	}
	if _, err := io.WriteString(w, submitEnter); err != nil {
		return fmt.Errorf("submit prompt: %w", err)
	}
	return nil
}

func (i *Injector) writeRune(w io.Writer, r rune) error {
	if r == '\n' {
		if _, err := io.WriteString(w, newlineWithoutSubmit); err != nil {
			return fmt.Errorf("write multiline newline: %w", err)
		}
		return nil
	}
	if _, err := io.WriteString(w, string(r)); err != nil {
		return fmt.Errorf("write prompt rune: %w", err)
	}
	return nil
}

func (i *Injector) perRuneDelay() time.Duration {
	wpm := i.cfg.WPM
	if wpm <= 0 {
		wpm = defaultWPM
	}
	return time.Minute / time.Duration(wpm*defaultCharsPerWord)
}

func (i *Injector) jitteredDelay() time.Duration {
	base := i.perRuneDelay()
	if i.cfg.Jitter <= 0 {
		return base
	}
	spread := float64(base) * i.cfg.Jitter
	factor := i.cfg.Rand()*2 - 1
	delay := float64(base) + spread*factor
	if delay < 0 {
		return 0
	}
	return time.Duration(delay)
}

// withDefaults fills unset fields. Note: Jitter is special — 0 means "no jitter"
// and is honored; only negative values get clamped to 0. The CLI default of 0.20
// is supplied at the options layer, so explicit 0 from the user disables jitter.
func (c Config) withDefaults() Config {
	if c.WPM <= 0 {
		c.WPM = defaultWPM
	}
	if c.Jitter < 0 {
		c.Jitter = 0
	}
	if c.SettleDelay <= 0 {
		c.SettleDelay = defaultSettleDelay
	}
	if c.WarnThreshold <= 0 {
		c.WarnThreshold = defaultWarnThreshold
	}
	if c.Sleeper == nil {
		c.Sleeper = c.realSleep
	}
	if c.Rand == nil {
		c.Rand = c.randFloat64
	}
	return c
}

func (Config) realSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("context done: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (Config) randFloat64() float64 {
	return rand.Float64() //nolint:gosec // typing jitter does not need cryptographic randomness.
}
