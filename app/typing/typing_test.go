package typing

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEstimate(t *testing.T) {
	got := NewInjector(Config{WPM: 100, Jitter: -1, SettleDelay: time.Second}).estimate("hello")

	assert.Equal(t, 5, got.characters)
	assert.Equal(t, 120*time.Millisecond, got.perRune)
	assert.Equal(t, 1600*time.Millisecond, got.total)
}

func TestValidateTimeoutGuard(t *testing.T) {
	err := NewInjector(Config{WPM: 10, TurnTimeout: time.Millisecond}).validate("this is too long")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds turn timeout")
}

func TestValidateWarning(t *testing.T) {
	var warn bytes.Buffer

	err := NewInjector(Config{WPM: 10, WarnThreshold: time.Millisecond, Warn: &warn}).validate("long prompt")

	require.NoError(t, err)
	assert.Contains(t, warn.String(), "estimated prompt typing duration")
}

func TestTypePrompt(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{
		WPM: 100, Jitter: -1, SettleDelay: time.Millisecond, Sleeper: sleep.sleep,
	}).Type(t.Context(), &out, "hi")

	require.NoError(t, err)
	assert.Equal(t, "hi\r", out.String())
	assert.Len(t, sleep.delays, 3, "two per-rune sleeps plus one settle delay")
}

func TestTypeMultilinePrompt(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{Jitter: -1, Sleeper: sleep.sleep}).Type(t.Context(), &out, "a\nb")

	require.NoError(t, err)
	assert.Equal(t, "a\x1b\rb\r", out.String())
}

func TestTypePasteAboveThreshold(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{MaxWPMSize: 2, SettleDelay: time.Millisecond, Sleeper: sleep.sleep}).Type(t.Context(), &out, "one two three")

	require.NoError(t, err)
	assert.Equal(t, "\x1b[200~one two three\x1b[201~\r", out.String(), "paste is wrapped in bracketed-paste markers then submitted")
	assert.Len(t, sleep.delays, 1, "paste mode uses only the settle delay, no per-rune sleeps")
}

func TestTypePasteMultiline(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{MaxWPMSize: 1, Sleeper: sleep.sleep}).Type(t.Context(), &out, "a\nbc")

	require.NoError(t, err)
	assert.Equal(t, "\x1b[200~a\nbc\x1b[201~\r", out.String(), "internal newline stays literal inside the bracketed paste so the prompt is one message")
	assert.Len(t, sleep.delays, 1, "single settle delay proves the paste path, not rune-by-rune typing")
}

func TestTypePasteAtThresholdStillTypes(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{WPM: 100, Jitter: -1, MaxWPMSize: 2, SettleDelay: time.Millisecond, Sleeper: sleep.sleep}).
		Type(t.Context(), &out, "a b")

	require.NoError(t, err)
	assert.Equal(t, "a b\r", out.String())
	assert.Len(t, sleep.delays, 4, "word count equal to threshold still types rune-by-rune (3 runes + settle)")
}

func TestTypePasteDisabledByDefault(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{WPM: 100, Jitter: -1, SettleDelay: time.Millisecond, Sleeper: sleep.sleep}).Type(t.Context(), &out, "a b c d")

	require.NoError(t, err)
	assert.Len(t, sleep.delays, 8, "MaxWPMSize=0 disables paste, all runes typed (7 runes + settle)")
}

func TestTypeForcePasteIgnoresThreshold(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{ForcePaste: true, MaxWPMSize: 0, SettleDelay: time.Millisecond, Sleeper: sleep.sleep}).
		Type(t.Context(), &out, "one")

	require.NoError(t, err)
	assert.Equal(t, "\x1b[200~one\x1b[201~\r", out.String())
	assert.Len(t, sleep.delays, 1)
}

func TestTypePasteSkipsTurnTimeoutGuard(t *testing.T) {
	var out bytes.Buffer

	err := NewInjector(Config{MaxWPMSize: 1, TurnTimeout: time.Nanosecond, Sleeper: (&fakeSleeper{}).sleep}).
		Type(t.Context(), &out, "a long prompt that would exceed any tiny typing budget")

	require.NoError(t, err, "paste mode must not hit the estimated-typing-duration turn-timeout guard")
}

func TestTypePasteWriteError(t *testing.T) {
	w := &errWriter{failAfter: 0}

	err := NewInjector(Config{MaxWPMSize: 1, Sleeper: (&fakeSleeper{}).sleep}).Type(t.Context(), w, "a b c")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "paste prompt")
}

func TestTypePasteSubmitWriteError(t *testing.T) {
	w := &errWriter{failAfter: 1}

	err := NewInjector(Config{MaxWPMSize: 1, Sleeper: (&fakeSleeper{}).sleep}).Type(t.Context(), w, "a b c")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "submit prompt")
}

func TestTypePasteSettleCancellation(t *testing.T) {
	err := NewInjector(Config{MaxWPMSize: 1, Sleeper: cancelSleep}).Type(t.Context(), &bytes.Buffer{}, "a b c")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "settle delay")
}

func TestNewInjectorNegativeMaxWPMSizeDisablesPaste(t *testing.T) {
	sleep := &fakeSleeper{}
	var out bytes.Buffer

	err := NewInjector(Config{WPM: 100, Jitter: -1, MaxWPMSize: -1, SettleDelay: time.Millisecond, Sleeper: sleep.sleep}).
		Type(t.Context(), &out, "a b c")

	require.NoError(t, err)
	assert.Len(t, sleep.delays, 6, "negative MaxWPMSize clamps to 0 (paste disabled), so all runes type (5 runes + settle)")
}

func TestJitterBounds(t *testing.T) {
	jitter := &sequenceJitter{values: []float64{0, 1, 0.5}}
	injector := NewInjector(Config{
		WPM:    100,
		Jitter: 0.25,
		Rand:   jitter.next,
	})

	assert.Equal(t, 90*time.Millisecond, injector.jitteredDelay(), "Rand=0 → base - spread")
	assert.Equal(t, 150*time.Millisecond, injector.jitteredDelay(), "Rand=1 → base + spread")
	assert.Equal(t, 120*time.Millisecond, injector.jitteredDelay(), "Rand=0.5 → base")
}

// explicit Jitter=0 must produce the base per-rune delay with no random offset,
// proving the flag --typing-jitter=0 actually disables jitter at runtime.
func TestJitterZeroHonored(t *testing.T) {
	jitter := &sequenceJitter{values: []float64{0.999}}
	injector := NewInjector(Config{
		WPM:    100,
		Jitter: 0,
		Rand:   jitter.next, // would shift if jitter applied
	})

	assert.Equal(t, 120*time.Millisecond, injector.jitteredDelay(), "Jitter=0 must disable jitter")
}

func TestDefaultJitterFunction(t *testing.T) {
	got := NewInjector(Config{WPM: 100, Jitter: 0.25}).jitteredDelay()

	assert.True(t, got >= 90*time.Millisecond && got <= 150*time.Millisecond, "default jitter delay out of range: %s", got)
}

func TestTypeRuneWriteError(t *testing.T) {
	w := &errWriter{failAfter: 0}
	sleep := &fakeSleeper{}

	err := NewInjector(Config{Jitter: -1, Sleeper: sleep.sleep}).Type(t.Context(), w, "x")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "write prompt rune")
}

func TestTypeNewlineWriteError(t *testing.T) {
	w := &errWriter{failAfter: 0}
	sleep := &fakeSleeper{}

	err := NewInjector(Config{Jitter: -1, Sleeper: sleep.sleep}).Type(t.Context(), w, "\n")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "write multiline newline")
}

func TestTypeSubmitWriteError(t *testing.T) {
	w := &errWriter{failAfter: 1}
	sleep := &fakeSleeper{}

	err := NewInjector(Config{Jitter: -1, Sleeper: sleep.sleep}).Type(t.Context(), w, "x")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "submit prompt")
}

func TestTypeDelayCancellation(t *testing.T) {
	err := NewInjector(Config{Sleeper: cancelSleep}).Type(t.Context(), &bytes.Buffer{}, "x")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "typing delay")
}

func TestDefaultSleeperFunctionHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer

	err := NewInjector(Config{}).Type(ctx, &out, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "settle delay")
	assert.Contains(t, err.Error(), "context canceled")
	assert.Empty(t, out.String())
}

func TestTypeNilWriter(t *testing.T) {
	err := NewInjector(Config{}).Type(t.Context(), nil, "x")

	require.Error(t, err)
}

type fakeSleeper struct {
	delays []time.Duration
}

func (s *fakeSleeper) sleep(_ context.Context, d time.Duration) error {
	s.delays = append(s.delays, d)
	return nil
}

func cancelSleep(context.Context, time.Duration) error {
	return context.Canceled
}

type sequenceJitter struct {
	values []float64
	idx    int
}

func (j *sequenceJitter) next() float64 {
	if j.idx >= len(j.values) {
		return 0.5
	}
	value := j.values[j.idx]
	j.idx++
	return value
}

type errWriter struct {
	failAfter int
	writes    int
}

func (w *errWriter) Write(p []byte) (int, error) {
	if w.writes >= w.failAfter {
		return 0, errors.New("write failed")
	}
	w.writes++
	return len(p), nil
}
