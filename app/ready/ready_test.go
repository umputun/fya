package ready

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/fya/app/ready/mocks"
)

func TestDetectorGlyphReadiness(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Claude\n> ")

	got, err := NewDetector(testConfig()).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "glyph", got.Method)
}

// the 2.1.214 editor prompt is the arrow glyph (U+276F); with no marker
// configured, its line-anchored form promotes readiness via the glyph path.
func TestDetectorArrowGlyphReadiness(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Claude\n❯ ")

	got, err := NewDetector(testConfig()).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "glyph", got.Method)
}

// defense-in-depth: the arrow-prompt glyph appearing inside a known blocking
// dialog must not promote readiness — the dialog veto runs before the glyph
// check, so we never type into the wrong UI.
func TestDetectorBlockingPromptVetoesArrowGlyph(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Do you trust the files in this folder?\n❯ ") // contains "\n❯ " glyph AND trust blocker

	got, err := NewDetector(Config{
		Timeout:         20 * time.Millisecond,
		QuietPeriod:     time.Second,
		PollInterval:    time.Millisecond,
		Warn:            &bytes.Buffer{},
		NonFatalTimeout: true,
	}).Wait(t.Context(), src)

	require.Error(t, err)
	assert.NotEqual(t, "glyph", got.Method, "blocking dialog veto must override the arrow-prompt glyph")
	assert.Equal(t, "timeout", got.Method, "should fall through to timeout when veto fires")
	assert.Contains(t, err.Error(), "blocked by prompt")
}

func TestDetectorQuietFallback(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Claude loading")

	got, err := NewDetector(Config{
		Timeout:      time.Second,
		QuietPeriod:  10 * time.Millisecond,
		PollInterval: time.Millisecond,
	}).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "quiet", got.Method)
}

func TestDetectorQuietFallbackResetsOnOutputChange(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Claude loading 1")

	state.readCh = make(chan struct{})
	started := time.Now()
	done := make(chan Result, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := NewDetector(Config{
			Timeout:      time.Second,
			QuietPeriod:  40 * time.Millisecond,
			PollInterval: 5 * time.Millisecond,
		}).Wait(t.Context(), src)
		done <- got
		errs <- err
	}()

	<-state.readCh
	time.Sleep(20 * time.Millisecond)
	state.setOutput("Claude loading 2")

	got := <-done
	err := <-errs
	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "quiet", got.Method)
	assert.Equal(t, "Claude loading 2", got.Output)
	assert.GreaterOrEqual(t, time.Since(started), 55*time.Millisecond)
}

func TestDetectorTimeoutWarning(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Claude is still loading")
	var warn bytes.Buffer

	got, err := NewDetector(Config{
		Timeout:         5 * time.Millisecond,
		QuietPeriod:     time.Second,
		PollInterval:    time.Millisecond,
		Warn:            &warn,
		NonFatalTimeout: true,
	}).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.False(t, got.Ready)
	assert.Equal(t, "timeout", got.Method)
	assert.Contains(t, warn.String(), "readiness timeout")
	assert.Contains(t, warn.String(), "captured Claude terminal output")
	assert.Contains(t, warn.String(), "Claude is still loading")
}

// without NonFatalTimeout, an expired Timeout must return an error so the caller
// can fail the turn instead of typing into a not-yet-ready editor.
func TestDetectorTimeoutFatal(t *testing.T) {
	src, _ := newMockSource()

	_, err := NewDetector(Config{
		Timeout:      5 * time.Millisecond,
		QuietPeriod:  time.Second,
		PollInterval: time.Millisecond,
	}).Wait(t.Context(), src)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "claude readiness timeout")
}

// a static trust/setup dialog must NOT be promoted to ready by the quiet
// fallback — fya would type the prompt into the wrong UI. The detector should
// keep waiting (and eventually time out) instead.
func TestDetectorQuietFallbackVetoedByBlockingPrompt(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Do you trust the files in this folder?")
	var warn bytes.Buffer

	got, err := NewDetector(Config{
		Timeout:         20 * time.Millisecond,
		QuietPeriod:     time.Millisecond,
		PollInterval:    time.Millisecond,
		Warn:            &warn,
		NonFatalTimeout: true,
	}).Wait(t.Context(), src)

	require.Error(t, err)
	assert.Equal(t, "timeout", got.Method, "quiet fallback must NOT activate on blocking dialog text")
	assert.NotEqual(t, "quiet", got.Method)
	assert.Contains(t, err.Error(), "blocked by prompt")
}

// defense-in-depth: even if a glyph string appears as a substring of a known
// blocking dialog, the dialog veto must win so we don't type into the wrong UI.
func TestDetectorBlockingPromptVetoesGlyph(t *testing.T) {
	src, state := newMockSource()
	// configure both a glyph and a blocking prompt where the dialog text
	// happens to contain the glyph string.
	state.setOutput("Do you trust the files in this folder?\n> ") // contains "\n> " glyph AND trust blocker

	got, err := NewDetector(Config{
		Timeout:         20 * time.Millisecond,
		QuietPeriod:     time.Second,
		PollInterval:    time.Millisecond,
		Warn:            &bytes.Buffer{},
		NonFatalTimeout: true,
	}).Wait(t.Context(), src)

	require.Error(t, err)
	assert.NotEqual(t, "glyph", got.Method, "blocking dialog veto must override glyph match")
	assert.Equal(t, "timeout", got.Method, "should fall through to timeout when veto fires")
	assert.Contains(t, err.Error(), "blocked by prompt")
}

func TestDetectorPermissionsBannerDoesNotBlockGlyph(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Bypassing Permissions\n> ")

	got, err := NewDetector(testConfig()).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "glyph", got.Method)
}

func TestDetectorCopiesCustomSlices(t *testing.T) {
	glyphs := []string{"READY"}
	blockers := []string{"BLOCKED"}
	detector := NewDetector(Config{Glyphs: glyphs, BlockingPrompts: blockers})
	glyphs[0] = "MUTATED"
	blockers[0] = "MUTATED"

	assert.Equal(t, []string{"READY"}, detector.cfg.Glyphs)
	assert.Equal(t, []string{"BLOCKED"}, detector.cfg.BlockingPrompts)
}

func TestDetectorPreservesExplicitEmptyBlockingPrompts(t *testing.T) {
	detector := NewDetector(Config{BlockingPrompts: []string{}})

	assert.NotNil(t, detector.cfg.BlockingPrompts)
	assert.Empty(t, detector.cfg.BlockingPrompts)
}

func TestDetectorProcessExitBeforeReady(t *testing.T) {
	src, state := newMockSource()
	state.close()

	got, err := NewDetector(testConfig()).Wait(t.Context(), src)

	require.Error(t, err)
	assert.Equal(t, "process-exit", got.Method)
}

func TestDetectorProcessExitBeforeReadyOutput(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Claude\n> ")
	state.close()

	got, err := NewDetector(testConfig()).Wait(t.Context(), src)

	require.Error(t, err)
	assert.Equal(t, "process-exit", got.Method)
}

func TestDetectorContextCancel(t *testing.T) {
	src, _ := newMockSource()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NewDetector(testConfig()).Wait(ctx, src)

	require.Error(t, err)
}

func TestDetectorNilSource(t *testing.T) {
	_, err := NewDetector(testConfig()).Wait(t.Context(), nil)

	require.Error(t, err)
}

func TestDetectorNilContext(t *testing.T) {
	src, _ := newMockSource()
	_, err := NewDetector(testConfig()).Wait(context.Context(nil), src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is nil")
}

func TestDetectorInputReadyMarker(t *testing.T) {
	src, state := newMockSource()
	// the input-ready marker is present and the output is stable, so readiness
	// fires on the marker once the quiet window has elapsed.
	state.setOutput("\x1b[?2004hidle prompt")

	started := time.Now()
	got, err := NewDetector(Config{
		Timeout:          time.Second,
		QuietPeriod:      10 * time.Millisecond,
		PollInterval:     time.Millisecond,
		InputReadyMarker: DefaultInputReadyMarker,
	}).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "input-ready", got.Method)
	assert.Equal(t, "\x1b[?2004hidle prompt", got.Output)
	// readiness must wait out the quiet window, not fire on the marker alone.
	assert.GreaterOrEqual(t, time.Since(started), 10*time.Millisecond)
}

// a marker seen while output is still changing (startup paint) must NOT fire
// readiness; the detector holds until the output settles, and here it never does
// so it times out. Firing on the marker alone would promote a still-painting
// editor and drop the opening prompt characters.
func TestDetectorInputReadyMarkerHeldDuringPaint(t *testing.T) {
	done := make(chan struct{})
	var n int
	src := &mocks.SourceMock{
		OutputFunc: func() string {
			n++
			return fmt.Sprintf("\x1b[?2004hpaint %d", n) // marker present, text never stable
		},
		DoneFunc: func() <-chan struct{} { return done },
	}

	got, err := NewDetector(Config{
		Timeout:          30 * time.Millisecond,
		QuietPeriod:      10 * time.Millisecond,
		PollInterval:     time.Millisecond,
		Warn:             &bytes.Buffer{},
		NonFatalTimeout:  true,
		InputReadyMarker: DefaultInputReadyMarker,
	}).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.Equal(t, "timeout", got.Method, "marker seen during ongoing paint must not fire readiness")
	assert.NotEqual(t, "input-ready", got.Method)
}

// with a marker present the whole time, readiness fires only after the output
// stops changing for QuietPeriod. Mirrors TestDetectorQuietFallbackResetsOnOutputChange:
// the output changes once mid-wait, so the earliest fire is QuietPeriod after
// that change.
func TestDetectorInputReadyMarkerFiresAfterOutputStabilizes(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("\x1b[?2004hpaint 1")

	state.readCh = make(chan struct{})
	started := time.Now()
	done := make(chan Result, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := NewDetector(Config{
			Timeout:          time.Second,
			QuietPeriod:      40 * time.Millisecond,
			PollInterval:     5 * time.Millisecond,
			InputReadyMarker: DefaultInputReadyMarker,
		}).Wait(t.Context(), src)
		done <- got
		errs <- err
	}()

	<-state.readCh
	time.Sleep(20 * time.Millisecond)
	state.setOutput("\x1b[?2004hpaint 2")

	got := <-done
	err := <-errs
	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "input-ready", got.Method)
	assert.Equal(t, "\x1b[?2004hpaint 2", got.Output)
	assert.GreaterOrEqual(t, time.Since(started), 55*time.Millisecond)
}

// the input-ready marker appears early and is then evicted from the capped 64 KiB
// PTY tail buffer during a long startup paint: the current output no longer
// contains the marker by the time it settles. Because the Wait loop latches
// "marker seen", readiness must still fire once the output stabilizes — dropping
// the marker from the later output is a faithful, deterministic stand-in for
// eviction. A marker seen once must keep readiness reachable even after it leaves
// the current buffer snapshot.
func TestDetectorInputReadyMarkerLatchesAcrossBufferEviction(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("\x1b[?2004hpaint 1") // marker present in the first read, latching markerSeen

	state.readCh = make(chan struct{})
	started := time.Now()
	done := make(chan Result, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := NewDetector(Config{
			Timeout:          time.Second,
			QuietPeriod:      40 * time.Millisecond,
			PollInterval:     5 * time.Millisecond,
			InputReadyMarker: DefaultInputReadyMarker,
		}).Wait(t.Context(), src)
		done <- got
		errs <- err
	}()

	<-state.readCh
	time.Sleep(20 * time.Millisecond)
	// the early marker has been evicted: the current output no longer contains it,
	// and never will again. Readiness must still fire off the latched sighting.
	state.setOutput("settled prompt without marker")

	got := <-done
	err := <-errs
	require.NoError(t, err)
	assert.True(t, got.Ready)
	assert.Equal(t, "input-ready", got.Method)
	assert.Equal(t, "settled prompt without marker", got.Output)
	assert.GreaterOrEqual(t, time.Since(started), 55*time.Millisecond)
}

// the real Claude trust dialog positions every word with cursor-move escapes and
// also emits the input-ready marker. The blocking veto must still recognize the
// dialog (via normalized matching) and win over the marker, so fya never types
// into it.
func TestDetectorBlockingPromptVetoesInputReadyWhenColumnPositioned(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("\x1b[?2004h\x1b[2GQuick\x1b[8Gsafety\x1b[15Gcheck:\x1b[22GIs\x1b[25Gthis\x1b[30Ga" +
		"\x1b[32Gproject\x1b[40Gyou\x1b[44Gcreated\x1b[52Gor\x1b[55Gone\x1b[59Gyou\x1b[63Gtrust?")

	// QuietPeriod is small so the stable marker output would fire input-ready were
	// the inspect-time veto absent — the veto is therefore the sole reason this
	// times out, keeping the test load-bearing on the veto.
	got, err := NewDetector(Config{
		Timeout:          20 * time.Millisecond,
		QuietPeriod:      time.Millisecond,
		PollInterval:     time.Millisecond,
		Warn:             &bytes.Buffer{},
		NonFatalTimeout:  true,
		InputReadyMarker: DefaultInputReadyMarker,
	}).Wait(t.Context(), src)

	require.Error(t, err)
	assert.NotEqual(t, "input-ready", got.Method, "blocking dialog must override the input-ready marker")
	assert.Equal(t, "timeout", got.Method)
	assert.Contains(t, err.Error(), "blocked by prompt")
}

func TestDetectorNormalizeForMatch(t *testing.T) {
	d := NewDetector(Config{})
	tests := []struct {
		name, in, want string
	}{
		{"plain", "abc", "abc"},
		{"whitespace", "a b\tc\nd", "abcd"},
		{"csi cursor move", "Quick\x1b[8Gsafety", "Quicksafety"},
		{"sgr color", "\x1b[38;2;1;2;3mhi\x1b[39m", "hi"},
		{"osc bel-terminated", "\x1b]11;?\x07x", "x"},
		{"osc st-terminated", "\x1b]11;rgb\x1b\\x", "x"},
		{"input-ready marker stripped clean", "\x1b[?2004hHello", "Hello"},
		{"lone esc retained", "a\x1bb", "a\x1bb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, d.normalizeForMatch(tt.in))
		})
	}
}

// the input-ready marker gates the weaker fallbacks: identical stable,
// glyph-free output is held back when a marker is configured but absent, yet
// promoted to "quiet" when no marker is configured. Varying only the marker
// isolates the gate as the cause.
func TestDetectorMarkerGatesFallbacks(t *testing.T) {
	wait := func(marker string) (Result, error) {
		src, state := newMockSource()
		state.setOutput("Claude loading")
		return NewDetector(Config{
			Timeout:          15 * time.Millisecond,
			QuietPeriod:      time.Millisecond,
			PollInterval:     time.Millisecond,
			Warn:             &bytes.Buffer{},
			NonFatalTimeout:  true,
			InputReadyMarker: marker,
		}).Wait(t.Context(), src)
	}

	t.Run("quiet gated when marker configured", func(t *testing.T) {
		got, err := wait(DefaultInputReadyMarker)
		require.NoError(t, err)
		assert.Equal(t, "timeout", got.Method)
	})
	t.Run("quiet fires when marker disabled", func(t *testing.T) {
		got, err := wait("")
		require.NoError(t, err)
		assert.Equal(t, "quiet", got.Method)
	})
}

// with a marker configured, an editor glyph must NOT promote readiness before
// the marker appears — otherwise the rendered-UI race the marker closes leaks
// back in through the glyph path.
func TestDetectorGlyphGatedByMarker(t *testing.T) {
	src, state := newMockSource()
	state.setOutput("Claude\n> ") // glyph present, marker absent
	got, err := NewDetector(Config{
		Timeout:          15 * time.Millisecond,
		QuietPeriod:      time.Second,
		PollInterval:     time.Millisecond,
		Warn:             &bytes.Buffer{},
		NonFatalTimeout:  true,
		InputReadyMarker: DefaultInputReadyMarker,
	}).Wait(t.Context(), src)

	require.NoError(t, err)
	assert.Equal(t, "timeout", got.Method, "glyph must not fire before the marker while a marker is configured")
}

func TestDetectorBlocked(t *testing.T) {
	d := NewDetector(Config{InputReadyMarker: DefaultInputReadyMarker})
	// column-positioned accept option, as Claude renders it
	assert.True(t, d.Blocked("\x1b[4G\x1b[7GYes,\x1b[12GI\x1b[14Gtrust\x1b[20Gthis\x1b[25Gfolder"),
		"current trust-dialog accept option must be detected even with cursor positioning")
	assert.True(t, d.Blocked("Do you trust the files in this folder?"), "legacy trust phrase still matches")
	assert.False(t, d.Blocked("\x1b[?2004h\x1b[2G❯ ready for input"), "a normal ready editor is not blocking")
}

type sourceState struct {
	mu       sync.Mutex
	readOnce sync.Once
	output   string
	done     chan struct{}
	readCh   chan struct{}
}

func newMockSource() (*mocks.SourceMock, *sourceState) {
	state := &sourceState{done: make(chan struct{})}
	return &mocks.SourceMock{
		OutputFunc: state.outputValue,
		DoneFunc:   state.doneChan,
	}, state
}

func (s *sourceState) outputValue() string {
	if s.readCh != nil {
		s.readOnce.Do(func() { close(s.readCh) })
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

func (s *sourceState) doneChan() <-chan struct{} {
	return s.done
}

func (s *sourceState) setOutput(output string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output = output
}

func (s *sourceState) close() {
	close(s.done)
}

func testConfig() Config {
	return Config{
		Timeout:      50 * time.Millisecond,
		QuietPeriod:  time.Second,
		PollInterval: time.Millisecond,
	}
}
