package ready

import (
	"bytes"
	"context"
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
