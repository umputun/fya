// Package turn wires one ephemeral Claude PTY turn: start PTY, wait for
// readiness, type the prompt, then tail the transcript and emit stream events.
package turn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"

	"github.com/umputun/fya/app/ptyrun"
	"github.com/umputun/fya/app/ready"
	"github.com/umputun/fya/app/stream"
	"github.com/umputun/fya/app/transcript"
)

// ErrTurnTimeout marks fya's own wall-clock turn timeout. Downstream
// orchestrators can use this stable marker to classify the failed turn as a
// transient Claude continuation stall instead of a generic process failure.
var ErrTurnTimeout = errors.New("FYA_TRANSIENT_TIMEOUT: claude turn did not complete before fya turn timeout")

// ErrNoActivityTimeout marks fya's idle no-activity stall: the transcript
// produced no new activity for the configured window while the turn was not
// completable. It shares the FYA_TRANSIENT_TIMEOUT marker so orchestrators
// classify it as a transient stall and retry.
var ErrNoActivityTimeout = errors.New("FYA_TRANSIENT_TIMEOUT: claude produced no transcript activity before fya no-activity timeout")

//go:generate moq -out mocks/session.go -pkg mocks -skip-ensure -fmt goimports . Session
//go:generate moq -out mocks/process_starter.go -pkg mocks -skip-ensure -fmt goimports . ProcessStarter
//go:generate moq -out mocks/readiness.go -pkg mocks -skip-ensure -fmt goimports . Readiness
//go:generate moq -out mocks/injector.go -pkg mocks -skip-ensure -fmt goimports . Injector
//go:generate moq -out mocks/catalog.go -pkg mocks -skip-ensure -fmt goimports . Catalog
//go:generate moq -out mocks/tailer.go -pkg mocks -skip-ensure -fmt goimports . Tailer
//go:generate moq -out mocks/output.go -pkg mocks -skip-ensure -fmt goimports . Output

// Session represents the wrapped Claude PTY process. It implements ready.Source
// for readiness polling plus prompt-typing and lifecycle methods. Implementers
// MUST return a non-nil channel from Done() — selectTranscript and
// streamTranscript both rely on Done() to abort on process exit.
type Session interface {
	ready.Source
	io.Writer
	Close() error
	Wait() error
}

// ProcessStarter starts the Claude PTY process and returns a Session that owns
// its lifecycle.
type ProcessStarter interface {
	Start(context.Context, ptyrun.Config) (Session, error)
}

// Readiness blocks until the Source is ready to receive a typed prompt. Blocked
// re-checks a captured output snapshot for a blocking dialog, used after the
// settle pause to catch a dialog that finished rendering since readiness fired.
type Readiness interface {
	Wait(context.Context, ready.Source) (ready.Result, error)
	Blocked(output string) bool
}

// Injector types prompt rune-by-rune into the supplied writer.
type Injector interface {
	Type(context.Context, io.Writer, string) error
}

// Catalog locates the transcript JSONL file Claude is writing for the current cwd.
type Catalog interface {
	Select(cwd string, since time.Time, prompt string) (string, error)
}

// Tailer reads transcript records across successive polls. ReadNew returns
// parsed events plus a transcript file-activity flag since the previous read,
// including partial trailing JSONL and records ignored by higher layers.
type Tailer interface {
	ReadNew() ([]transcript.Event, bool, error)
}

// TailerFactory constructs a Tailer for a given transcript path.
type TailerFactory func(path string) Tailer

// Output writes Claude print-mode events and the final result.
type Output interface {
	Text(string) error
	Event(stream.Event) error
	Final(stream.Result) error
}

// Config controls one Runner.Run invocation. All event output goes through
// Dependencies.Output; stdout/stderr are owned by the caller of main.
type Config struct {
	ClaudeArgs        []string
	CWD               string
	TurnTimeout       time.Duration
	IdleTimeout       time.Duration
	NoActivityTimeout time.Duration
	StreamEvents      bool
	Prompt            string
	StartedAt         time.Time
	PollPeriod        time.Duration
	// TypeSettle pauses between readiness and typing the prompt. It is an extra
	// margin on top of the readiness gate for environments (e.g. a Docker Desktop
	// VM) whose terminal I/O lags behind the input-ready marker. Zero disables it.
	TypeSettle time.Duration
}

// Runner orchestrates a single Claude PTY turn through the injected dependencies.
type Runner struct {
	starter ProcessStarter
	ready   Readiness
	inject  Injector
	catalog Catalog
	tailers TailerFactory
	output  Output
	sleep   func(context.Context, time.Duration) error
	rand    func() float64
}

// NewRunner returns a Runner wired with deps; missing fields cause Run to fail
// fast with a clear error.
func NewRunner(deps Dependencies) *Runner {
	r := &Runner{
		starter: deps.ProcessStarter,
		ready:   deps.Readiness,
		inject:  deps.Injector,
		catalog: deps.Catalog,
		tailers: deps.TailerFactory,
		output:  deps.Output,
		sleep:   deps.Sleeper,
		rand:    deps.Rand,
	}
	if r.sleep == nil {
		r.sleep = r.realSleep
	}
	if r.rand == nil {
		r.rand = r.randFloat64
	}
	return r
}

// Dependencies groups the collaborators Runner needs.
type Dependencies struct {
	ProcessStarter ProcessStarter
	Readiness      Readiness
	Injector       Injector
	Catalog        Catalog
	TailerFactory  TailerFactory
	Output         Output
	// Sleeper waits for a duration or until ctx is canceled; tests inject a fake
	// so the TypeSettle pause is deterministic. Nil uses a real timer.
	Sleeper func(context.Context, time.Duration) error
	// Rand returns a uniform float in [0, 1) used to jitter the TypeSettle pause.
	// Nil uses a non-cryptographic real source.
	Rand func() float64
}

// Run executes one Claude turn: start the PTY, wait for readiness, type the
// prompt, then tail the transcript until the turn completes, the wall-clock
// turn-timeout fires, the parent context is canceled, or Claude exits.
func (r *Runner) Run(ctx context.Context, cfg Config) error {
	if err := r.validate(); err != nil {
		return err
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now()
	}
	if cfg.PollPeriod <= 0 {
		cfg.PollPeriod = 100 * time.Millisecond
	}

	// enforce --turn-timeout as a wall-clock deadline for the entire turn so a
	// hung Claude / missing result event / stalled transcript never blocks fya
	// indefinitely. The PTY driver's watchCancel will kill the process group.
	if cfg.TurnTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, cfg.TurnTimeout,
			fmt.Errorf("%w after %s", ErrTurnTimeout, cfg.TurnTimeout))
		defer cancel()
	}

	session, err := r.starter.Start(ctx, ptyrun.Config{
		Command: "claude",
		Args:    cfg.ClaudeArgs,
		Dir:     cfg.CWD,
	})
	if err != nil {
		if ctx.Err() != nil {
			return r.finishCanceled(ctx, "start claude pty")
		}
		return fmt.Errorf("start claude pty: %w", err)
	}
	defer r.cleanupSession(session)

	if _, readyErr := r.ready.Wait(ctx, session); readyErr != nil {
		if ctx.Err() != nil {
			return r.finishCanceled(ctx, "wait claude readiness")
		}
		return fmt.Errorf("wait claude readiness: %w", readyErr)
	}
	if cfg.TypeSettle > 0 {
		if settleErr := r.sleep(ctx, r.settleDelay(cfg.TypeSettle)); settleErr != nil {
			if ctx.Err() != nil {
				return r.finishCanceled(ctx, "settle before type")
			}
			return fmt.Errorf("settle before type: %w", settleErr)
		}
		// a column-positioned dialog (e.g. the trust prompt) can finish rendering
		// during the settle window, after readiness fired on the input-ready
		// marker. Re-check the fresh output so the prompt is never typed into it.
		if r.ready.Blocked(session.Output()) {
			return r.finishBlocked()
		}
	}
	if typeErr := r.inject.Type(ctx, session, cfg.Prompt); typeErr != nil {
		if ctx.Err() != nil {
			return r.finishCanceled(ctx, "type prompt")
		}
		return fmt.Errorf("type prompt: %w", typeErr)
	}

	path, err := r.selectTranscript(ctx, cfg, session.Done())
	if err != nil {
		if finalErr := r.output.Final(r.cancelResult("", ctx)); finalErr != nil {
			return fmt.Errorf("write final output: %w", finalErr)
		}
		return err
	}
	return r.streamTranscript(ctx, streamRequest{cfg: cfg, session: session, tailer: r.tailers(path)})
}

func (r *Runner) cleanupSession(session Session) {
	if err := session.Close(); err != nil {
		log.Printf("[WARN] close session: %v", err)
	}
	if err := session.Wait(); err != nil {
		log.Printf("[DEBUG] wait session after cleanup: %v", err)
	}
}

// selectTranscript polls catalog.Select until a transcript modified after
// StartedAt and containing the prompt appears, ctx is canceled, or Claude
// exits. Claude can take a moment to flush a new transcript so the loop
// tolerates ErrNoTranscript; watching sessionDone prevents a 30-minute wait if
// Claude crashed between typing and the first transcript flush.
func (r *Runner) selectTranscript(ctx context.Context, cfg Config, sessionDone <-chan struct{}) (string, error) {
	ticker := time.NewTicker(cfg.PollPeriod)
	defer ticker.Stop()
	for {
		path, err := r.catalog.Select(cfg.CWD, cfg.StartedAt, cfg.Prompt)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, transcript.ErrNoTranscript) {
			return "", fmt.Errorf("select transcript: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("select transcript: %w", r.ctxError(ctx))
		case <-sessionDone:
			return "", errors.New("select transcript: claude exited before transcript was written")
		case <-ticker.C:
		}
	}
}

type streamRequest struct {
	cfg     Config
	session Session
	tailer  Tailer
}

func (r *Runner) streamTranscript(ctx context.Context, req streamRequest) error {
	tracker := transcript.NewTracker()
	completion := transcript.Completion{IdleTimeout: req.cfg.IdleTimeout}
	ticker := time.NewTicker(req.cfg.PollPeriod)
	defer ticker.Stop()
	sessionDone := req.session.Done()
	lastTranscriptActivityAt := time.Now()
	var lastEvent transcript.Event
	var finalText strings.Builder
	var hasFinalText bool

	for {
		events, activity, err := req.tailer.ReadNew()
		if err != nil {
			if finalErr := r.output.Final(stream.Result{SessionID: lastEvent.SessionID, IsError: true, Subtype: "error"}); finalErr != nil {
				return fmt.Errorf("write final output: %w", finalErr)
			}
			return fmt.Errorf("read transcript: %w", err)
		}
		if activity || len(events) > 0 {
			lastTranscriptActivityAt = time.Now()
		}
		state := applyState{
			tracker:      tracker,
			lastEvent:    &lastEvent,
			completion:   completion,
			streamEvents: req.cfg.StreamEvents,
			finalText:    &finalText,
			hasFinalText: &hasFinalText,
		}
		done, err := r.applyEvents(events, state)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if completion.Done(tracker, lastEvent, time.Since(lastTranscriptActivityAt)) {
			if err := r.output.Final(state.finalResult(lastEvent.SessionID)); err != nil {
				return fmt.Errorf("write final output: %w", err)
			}
			return nil
		}
		// context cancellation (turn timeout / parent) and an already-exited claude
		// both take precedence over the stall: a canceled turn must keep its real
		// terminal reason, and a final result may still be draining
		if req.cfg.NoActivityTimeout > 0 && ctx.Err() == nil && !completion.Eligible(tracker, lastEvent) &&
			time.Since(lastTranscriptActivityAt) >= req.cfg.NoActivityTimeout {
			select {
			case <-sessionDone:
				return r.handleSessionExit(ctx, req.tailer, state)
			default:
			}
			cause := fmt.Errorf("%w after %s", ErrNoActivityTimeout, req.cfg.NoActivityTimeout)
			if err := r.output.Final(r.noActivityResult(lastEvent.SessionID, cause)); err != nil {
				return fmt.Errorf("write final output: %w", err)
			}
			return fmt.Errorf("turn canceled: %w", cause)
		}
		select {
		case <-ctx.Done():
			if err := r.output.Final(r.cancelResult(lastEvent.SessionID, ctx)); err != nil {
				return fmt.Errorf("write final output: %w", err)
			}
			return fmt.Errorf("turn canceled: %w", r.ctxError(ctx))
		case <-sessionDone:
			return r.handleSessionExit(ctx, req.tailer, state)
		case <-ticker.C:
		}
	}
}

// applyState groups the mutable accumulators a batch of events is folded into.
type applyState struct {
	tracker      *transcript.Tracker
	lastEvent    *transcript.Event
	completion   transcript.Completion
	streamEvents bool
	finalText    *strings.Builder
	hasFinalText *bool
}

func (s applyState) trackFinalText(event transcript.Event) {
	if s.finalText == nil || s.hasFinalText == nil {
		return
	}
	if event.StopReason == "tool_use" || len(event.ToolUseIDs) > 0 {
		s.finalText.Reset()
		*s.hasFinalText = true
		return
	}
	if event.Text == "" {
		return
	}
	s.finalText.WriteString(event.Text)
	*s.hasFinalText = true
}

func (s applyState) finalResult(sessionID string) stream.Result {
	result := stream.Result{SessionID: sessionID}
	if s.finalText == nil || s.hasFinalText == nil {
		return result
	}
	result.FinalText = s.finalText.String()
	result.HasFinalText = *s.hasFinalText
	return result
}

// applyEvents folds a batch of events through the tracker, emits text deltas,
// and returns done=true if any event completes the turn (Final emitted). The
// idle-timeout completion check happens once per tick in the caller; here we
// only ever care about per-event terminal signals (e.g. a result event).
func (r *Runner) applyEvents(events []transcript.Event, s applyState) (bool, error) {
	for _, event := range events {
		*s.lastEvent = event
		s.tracker.Apply(event)
		s.trackFinalText(event)
		emittedEvent := false
		if s.streamEvents && len(event.Message) > 0 {
			if err := r.output.Event(stream.Event{Type: event.Type, SessionID: event.SessionID, Message: event.Message}); err != nil {
				return false, fmt.Errorf("write output event: %w", err)
			}
			emittedEvent = true
		}
		if event.Text != "" && !emittedEvent {
			if err := r.output.Text(event.Text); err != nil {
				return false, fmt.Errorf("write output text: %w", err)
			}
		}
		if s.completion.Done(s.tracker, event, 0) {
			if err := r.output.Final(s.finalResult(event.SessionID)); err != nil {
				return false, fmt.Errorf("write final output: %w", err)
			}
			return true, nil
		}
	}
	return false, nil
}

// drainRetries / drainRetryDelay bound how patiently handleSessionExit waits
// for Claude's last writes to land on disk after process exit. Without retries
// a successful turn whose result event was written immediately before exit
// could be observed as zero drained events and falsely reported as IsError.
const (
	// The values give a modest post-exit window for final transcript lines that
	// may still be buffered in the kernel when the Claude process terminates.
	// They remain small so a genuinely truncated turn still surfaces an error
	// without long hangs.
	drainRetries    = 8
	drainRetryDelay = 25 * time.Millisecond
)

// handleSessionExit drains the tailer one last time after Claude has exited. It
// retries a few times to absorb the OS page-cache flush window: a result event
// written just before exit may not be visible on the first ReadNew. If the
// drained events ever complete the turn the runner emits a normal Final and
// returns nil; only when no completion data appears does it emit an is_error
// Final.
func (r *Runner) handleSessionExit(ctx context.Context, tailer Tailer, s applyState) error {
	for attempt := range drainRetries {
		drained, _, err := tailer.ReadNew()
		if err != nil {
			if finalErr := r.output.Final(stream.Result{SessionID: s.lastEvent.SessionID, IsError: true, Subtype: "error"}); finalErr != nil {
				return fmt.Errorf("write final output after session exit: %w", finalErr)
			}
			return fmt.Errorf("read transcript after session exit: %w", err)
		}
		done, applyErr := r.applyEvents(drained, s)
		if applyErr != nil {
			return applyErr
		}
		if done {
			return nil
		}
		if attempt+1 < drainRetries {
			select {
			case <-ctx.Done():
				if err := r.output.Final(r.cancelResult(s.lastEvent.SessionID, ctx)); err != nil {
					return fmt.Errorf("write final output after session exit: %w", err)
				}
				return fmt.Errorf("turn canceled: %w", r.ctxError(ctx))
			case <-time.After(drainRetryDelay):
			}
		}
	}
	if s.completion.Done(s.tracker, *s.lastEvent, s.completion.IdleTimeout) {
		if err := r.output.Final(s.finalResult(s.lastEvent.SessionID)); err != nil {
			return fmt.Errorf("write final output after session exit: %w", err)
		}
		return nil
	}
	if err := r.output.Final(stream.Result{
		SessionID: s.lastEvent.SessionID,
		IsError:   true,
		Subtype:   "error",
	}); err != nil {
		return fmt.Errorf("write final output after session exit: %w", err)
	}
	return errors.New("claude exited before turn completion")
}

// finishBlocked aborts the turn when a blocking dialog appears during the settle
// window: it emits an error final result and returns an error instead of typing
// the prompt into the dialog.
func (r *Runner) finishBlocked() error {
	msg := "blocking dialog appeared after settle; aborting before typing prompt"
	if err := r.output.Final(stream.Result{IsError: true, Subtype: "error", TerminalReason: "error", Result: msg}); err != nil {
		return fmt.Errorf("write final output: %w", err)
	}
	return errors.New(msg)
}

func (r *Runner) finishCanceled(ctx context.Context, op string) error {
	if err := r.output.Final(r.cancelResult("", ctx)); err != nil {
		return fmt.Errorf("write final output: %w", err)
	}
	return fmt.Errorf("%s: %w", op, r.ctxError(ctx))
}

func (r *Runner) cancelResult(sessionID string, ctx context.Context) stream.Result {
	result := stream.Result{SessionID: sessionID, IsError: true, Subtype: "error", TerminalReason: "error"}
	if cause := context.Cause(ctx); errors.Is(cause, ErrTurnTimeout) {
		result.TerminalReason = "fya_turn_timeout"
		result.Result = cause.Error()
	}
	return result
}

func (r *Runner) noActivityResult(sessionID string, cause error) stream.Result {
	return stream.Result{
		SessionID:      sessionID,
		IsError:        true,
		Subtype:        "error",
		TerminalReason: "fya_no_activity_timeout",
		Result:         cause.Error(),
	}
}

func (r *Runner) ctxError(ctx context.Context) error {
	err := ctx.Err()
	cause := context.Cause(ctx)
	if cause == nil || errors.Is(cause, err) {
		return fmt.Errorf("context error: %w", err)
	}
	return fmt.Errorf("context error: %w: %w", err, cause)
}

// realSleep waits for d or until ctx is canceled. It is the default Sleeper used
// when Dependencies.Sleeper is nil.
func (*Runner) realSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("context done: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// settleJitter is the upward randomization applied to the TypeSettle pause so it
// is not a constant, fingerprintable interval; the configured value is the floor.
const settleJitter = 0.2

// settleDelay returns d extended by up to settleJitter of itself, keeping the
// pause varied between invocations while never dropping below the configured
// minimum margin. The jitter factor is clamped to [0, 1] so a misbehaving Rand
// cannot produce a delay below the floor or an unbounded one.
func (r *Runner) settleDelay(d time.Duration) time.Duration {
	factor := r.rand()
	if factor < 0 {
		factor = 0
	}
	if factor >= 1 {
		factor = 1
	}
	return d + time.Duration(float64(d)*settleJitter*factor)
}

func (*Runner) randFloat64() float64 {
	return rand.Float64() //nolint:gosec // settle jitter does not need cryptographic randomness.
}

func (r *Runner) validate() error {
	switch {
	case r.starter == nil:
		return errors.New("process starter is nil")
	case r.ready == nil:
		return errors.New("readiness detector is nil")
	case r.inject == nil:
		return errors.New("typing injector is nil")
	case r.catalog == nil:
		return errors.New("transcript catalog is nil")
	case r.tailers == nil:
		return errors.New("transcript tailer factory is nil")
	case r.output == nil:
		return errors.New("output writer is nil")
	default:
		return nil
	}
}

// NewPTYStarter returns a ProcessStarter that builds a real ptyrun.Driver per
// invocation. This is the production wiring; tests use mocks generated by moq.
func NewPTYStarter() ProcessStarter {
	return ptyStarter{}
}

type ptyStarter struct{}

func (ptyStarter) Start(ctx context.Context, cfg ptyrun.Config) (Session, error) {
	session, err := ptyrun.NewDriver(cfg).Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("start pty driver: %w", err)
	}
	return session, nil
}
