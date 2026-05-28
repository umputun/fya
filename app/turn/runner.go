// Package turn wires one ephemeral Claude PTY turn: start PTY, wait for
// readiness, type the prompt, then tail the transcript and emit stream events.
package turn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	log "github.com/go-pkgz/lgr"

	"github.com/umputun/fya/app/ptyrun"
	"github.com/umputun/fya/app/ready"
	"github.com/umputun/fya/app/stream"
	"github.com/umputun/fya/app/transcript"
)

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

// Readiness blocks until the Source is ready to receive a typed prompt.
type Readiness interface {
	Wait(context.Context, ready.Source) (ready.Result, error)
}

// Injector types prompt rune-by-rune into the supplied writer.
type Injector interface {
	Type(context.Context, io.Writer, string) error
}

// Catalog locates the transcript JSONL file Claude is writing for the current cwd.
type Catalog interface {
	Select(cwd string, since time.Time, prompt string) (string, error)
}

// Tailer reads new events from a transcript file across successive polls.
type Tailer interface {
	ReadNew() ([]transcript.Event, error)
}

// TailerFactory constructs a Tailer for a given transcript path.
type TailerFactory func(path string) Tailer

// Output writes assistant text deltas and the final stream-json result.
type Output interface {
	Text(string) error
	Final(stream.Result) error
}

// Config controls one Runner.Run invocation. All event output goes through
// Dependencies.Output; stdout/stderr are owned by the caller of main.
type Config struct {
	ClaudeArgs  []string
	CWD         string
	TurnTimeout time.Duration
	IdleTimeout time.Duration
	Prompt      string
	StartedAt   time.Time
	PollPeriod  time.Duration
}

// Runner orchestrates a single Claude PTY turn through the injected dependencies.
type Runner struct {
	starter ProcessStarter
	ready   Readiness
	inject  Injector
	catalog Catalog
	tailers TailerFactory
	output  Output
}

// NewRunner returns a Runner wired with deps; missing fields cause Run to fail
// fast with a clear error.
func NewRunner(deps Dependencies) *Runner {
	return &Runner{
		starter: deps.ProcessStarter,
		ready:   deps.Readiness,
		inject:  deps.Injector,
		catalog: deps.Catalog,
		tailers: deps.TailerFactory,
		output:  deps.Output,
	}
}

// Dependencies groups the collaborators Runner needs.
type Dependencies struct {
	ProcessStarter ProcessStarter
	Readiness      Readiness
	Injector       Injector
	Catalog        Catalog
	TailerFactory  TailerFactory
	Output         Output
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
		ctx, cancel = context.WithTimeout(ctx, cfg.TurnTimeout)
		defer cancel()
	}

	session, err := r.starter.Start(ctx, ptyrun.Config{
		Command: "claude",
		Args:    cfg.ClaudeArgs,
		Dir:     cfg.CWD,
	})
	if err != nil {
		return fmt.Errorf("start claude pty: %w", err)
	}
	defer r.cleanupSession(session)

	if _, readyErr := r.ready.Wait(ctx, session); readyErr != nil {
		return fmt.Errorf("wait claude readiness: %w", readyErr)
	}
	if typeErr := r.inject.Type(ctx, session, cfg.Prompt); typeErr != nil {
		return fmt.Errorf("type prompt: %w", typeErr)
	}

	path, err := r.selectTranscript(ctx, cfg, session.Done())
	if err != nil {
		if finalErr := r.output.Final(stream.Result{IsError: true, Subtype: "error"}); finalErr != nil {
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
			return "", fmt.Errorf("select transcript: %w", ctx.Err())
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
	lastEventAt := time.Now()
	var lastEvent transcript.Event

	for {
		events, err := req.tailer.ReadNew()
		if err != nil {
			if finalErr := r.output.Final(stream.Result{SessionID: lastEvent.SessionID, IsError: true, Subtype: "error"}); finalErr != nil {
				return fmt.Errorf("write final output: %w", finalErr)
			}
			return fmt.Errorf("read transcript: %w", err)
		}
		if len(events) > 0 {
			lastEventAt = time.Now()
		}
		state := applyState{tracker: tracker, lastEvent: &lastEvent, completion: completion}
		done, err := r.applyEvents(events, state)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if completion.Done(tracker, lastEvent, time.Since(lastEventAt)) {
			if err := r.output.Final(stream.Result{SessionID: lastEvent.SessionID}); err != nil {
				return fmt.Errorf("write final output: %w", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			if err := r.output.Final(stream.Result{SessionID: lastEvent.SessionID, IsError: true, Subtype: "error"}); err != nil {
				return fmt.Errorf("write final output: %w", err)
			}
			return fmt.Errorf("turn canceled: %w", ctx.Err())
		case <-sessionDone:
			return r.handleSessionExit(ctx, req.tailer, state)
		case <-ticker.C:
		}
	}
}

// applyState groups the mutable accumulators a batch of events is folded into.
type applyState struct {
	tracker    *transcript.Tracker
	lastEvent  *transcript.Event
	completion transcript.Completion
}

// applyEvents folds a batch of events through the tracker, emits text deltas,
// and returns done=true if any event completes the turn (Final emitted). The
// idle-timeout completion check happens once per tick in the caller; here we
// only ever care about per-event terminal signals (e.g. a result event).
func (r *Runner) applyEvents(events []transcript.Event, s applyState) (bool, error) {
	for _, event := range events {
		*s.lastEvent = event
		s.tracker.Apply(event)
		if event.Text != "" {
			if err := r.output.Text(event.Text); err != nil {
				return false, fmt.Errorf("write output text: %w", err)
			}
		}
		if s.completion.Done(s.tracker, event, 0) {
			if err := r.output.Final(stream.Result{SessionID: event.SessionID}); err != nil {
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
		drained, err := tailer.ReadNew()
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
				if err := r.output.Final(stream.Result{SessionID: s.lastEvent.SessionID, IsError: true, Subtype: "error"}); err != nil {
					return fmt.Errorf("write final output after session exit: %w", err)
				}
				return fmt.Errorf("turn canceled: %w", ctx.Err())
			case <-time.After(drainRetryDelay):
			}
		}
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
