package turn_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/fya/app/ptyrun"
	"github.com/umputun/fya/app/ready"
	"github.com/umputun/fya/app/stream"
	"github.com/umputun/fya/app/transcript"
	"github.com/umputun/fya/app/turn"
	"github.com/umputun/fya/app/turn/mocks"
)

func TestRunnerSuccess(t *testing.T) {
	session := newSessionMock()
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{
			StartFunc: func(_ context.Context, cfg ptyrun.Config) (turn.Session, error) {
				assert.Equal(t, "claude", cfg.Command)
				assert.Equal(t, []string{"--verbose"}, cfg.Args)
				return session, nil
			},
		},
		Readiness: &mocks.ReadinessMock{
			WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
				return ready.Result{Ready: true}, nil
			},
		},
		Injector: &mocks.InjectorMock{
			TypeFunc: func(_ context.Context, _ io.Writer, prompt string) error {
				assert.Equal(t, "hello", prompt)
				return nil
			},
		},
		Catalog: &mocks.CatalogMock{
			SelectFunc: func(_ string, _ time.Time, prompt string) (string, error) {
				assert.Equal(t, "hello", prompt)
				return "session.jsonl", nil
			},
		},
		TailerFactory: func(path string) turn.Tailer {
			assert.Equal(t, "session.jsonl", path)
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				return []transcript.Event{{Text: "answer", Result: true, SessionID: "s1"}}, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		ClaudeArgs: []string{"--verbose"}, CWD: ".", IdleTimeout: time.Second, TurnTimeout: time.Minute,
		Prompt: "hello",
	})

	require.NoError(t, err)
	require.Len(t, output.TextCalls(), 1)
	assert.Equal(t, "answer", output.TextCalls()[0].S)
	require.Len(t, output.FinalCalls(), 1)
	assert.Equal(t, "s1", output.FinalCalls()[0].Result.SessionID)
	assert.Len(t, session.CloseCalls(), 1)
	assert.Len(t, session.WaitCalls(), 1)
}

func TestRunnerStartFailure(t *testing.T) {
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return nil, errors.New("boom")
		}},
		Readiness:     &mocks.ReadinessMock{},
		Injector:      &mocks.InjectorMock{},
		Catalog:       &mocks.CatalogMock{},
		TailerFactory: noopTailerFactory,
		Output:        &mocks.OutputMock{},
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "hello"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "start claude pty")
}

func TestRunnerCatalogPropagatesNonNoTranscriptError(t *testing.T) {
	runner := runnerWithCatalogError(errors.New("read transcript dir: boom"))

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "hello"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "select transcript")
}

// when the transcript isn't yet flushed, catalog.Select returns ErrNoTranscript;
// the runner must retry until one appears or the turn deadline / parent ctx
// cancels.
func TestRunnerSelectRetriesUntilCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			calls++
			if calls > 1 {
				cancel()
			}
			return "", transcript.ErrNoTranscript
		}},
		TailerFactory: noopTailerFactory,
		Output:        &mocks.OutputMock{FinalFunc: func(stream.Result) error { return nil }},
	})

	err := runner.Run(ctx, turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "hi", PollPeriod: time.Millisecond})

	require.Error(t, err)
	assert.GreaterOrEqual(t, calls, 2, "Select must be retried at least once on ErrNoTranscript")
}

func TestRunnerReadinessFailure(t *testing.T) {
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{}, errors.New("not ready")
		}},
		Injector:      &mocks.InjectorMock{},
		Catalog:       &mocks.CatalogMock{},
		TailerFactory: noopTailerFactory,
		Output:        &mocks.OutputMock{},
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "hi"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wait claude readiness")
}

func TestRunnerInjectFailure(t *testing.T) {
	session := newSessionMock()
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error {
			return errors.New("typing failed")
		}},
		Catalog:       &mocks.CatalogMock{},
		TailerFactory: noopTailerFactory,
		Output:        &mocks.OutputMock{},
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "hi"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "type prompt")
	assert.Len(t, session.CloseCalls(), 1)
	assert.Len(t, session.WaitCalls(), 1)
}

func TestRunnerInjectorWritesPromptToSession(t *testing.T) {
	var wrote string
	session := newSessionMock()
	session.WriteFunc = func(p []byte) (int, error) {
		wrote += string(p)
		return len(p), nil
	}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(_ context.Context, w io.Writer, _ string) error {
			_, err := w.Write([]byte("hello"))
			if err != nil {
				return fmt.Errorf("write prompt: %w", err)
			}
			return nil
		}},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				return []transcript.Event{{Result: true, SessionID: "s"}}, nil
			}}
		},
		Output: &mocks.OutputMock{TextFunc: func(string) error { return nil }, FinalFunc: func(stream.Result) error { return nil }},
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "ignored"})

	require.NoError(t, err)
	assert.Equal(t, "hello", wrote)
}

func TestRunnerInjectorWriteError(t *testing.T) {
	session := newSessionMock()
	session.WriteFunc = func([]byte) (int, error) { return 0, errors.New("write failed") }
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(_ context.Context, w io.Writer, _ string) error {
			_, err := w.Write([]byte("hello"))
			if err != nil {
				return fmt.Errorf("write prompt: %w", err)
			}
			return nil
		}},
		Catalog:       &mocks.CatalogMock{},
		TailerFactory: noopTailerFactory,
		Output:        &mocks.OutputMock{},
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "ignored"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "type prompt")
	assert.Contains(t, err.Error(), "write failed")
}

// when the tailer fails mid-stream, streamTranscript must surface the wrapped
// error rather than masking it as a silent completion.
func TestRunnerTailerError(t *testing.T) {
	output := &mocks.OutputMock{TextFunc: func(string) error { return nil }, FinalFunc: func(stream.Result) error { return nil }}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				return nil, errors.New("tail boom")
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "hi", PollPeriod: time.Millisecond})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read transcript")
	require.Len(t, output.FinalCalls(), 1)
	assert.True(t, output.FinalCalls()[0].Result.IsError)
}

// when output.Final fails, the error must propagate.
func TestRunnerFinalError(t *testing.T) {
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return errors.New("final boom") },
	}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				return []transcript.Event{{Text: "x", Result: true, SessionID: "s"}}, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "hi", PollPeriod: time.Millisecond})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "write final output")
}

// idle-timeout completion: assistant text arrives, then no events for IdleTimeout;
// the runner must emit Final and return without an explicit result event.
func TestRunnerIdleCompletion(t *testing.T) {
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				calls++
				if calls == 1 {
					return []transcript.Event{{Text: "answer", SessionID: "s2"}}, nil
				}
				return nil, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute, IdleTimeout: time.Millisecond,
		Prompt:     "hi",
		PollPeriod: 5 * time.Millisecond,
	})

	require.NoError(t, err)
	require.Len(t, output.FinalCalls(), 1)
	assert.Equal(t, "s2", output.FinalCalls()[0].Result.SessionID)
}

func TestRunnerEmitsProgressText(t *testing.T) {
	var texts []string
	output := &mocks.OutputMock{
		TextFunc: func(text string) error {
			texts = append(texts, text)
			return nil
		},
		FinalFunc: func(stream.Result) error { return nil },
	}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				return []transcript.Event{{Type: "assistant", ProgressText: "tool: Bash go test ./...\n", Result: true, SessionID: "s"}}, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute, IdleTimeout: time.Second, Progress: true,
		Prompt: "hi",
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"tool: Bash go test ./...\n"}, texts)
}

func TestRunnerEmitsStreamMessageEvents(t *testing.T) {
	var events []stream.Event
	output := &mocks.OutputMock{
		EventFunc: func(event stream.Event) error {
			events = append(events, event)
			return nil
		},
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				return []transcript.Event{{
					Type:         "assistant",
					Text:         "answer",
					ProgressText: "tool: Bash go test ./...\n",
					Message:      []byte(`{"role":"assistant","content":[{"type":"text","text":"answer"}]}`),
					Result:       true,
					SessionID:    "s",
				}}, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute, IdleTimeout: time.Second, Progress: true, StreamEvents: true,
		Prompt: "hi",
	})

	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "assistant", events[0].Type)
	assert.Equal(t, "s", events[0].SessionID)
	assert.Empty(t, output.TextCalls(), "raw stream events replace legacy text/progress deltas")
}

func TestRunnerSkipsProgressTextWhenDisabled(t *testing.T) {
	output := &mocks.OutputMock{TextFunc: func(string) error { return nil }, FinalFunc: func(stream.Result) error { return nil }}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				return []transcript.Event{{Type: "assistant", ProgressText: "tool: Bash go test ./...\n", Result: true, SessionID: "s"}}, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, IdleTimeout: time.Second, Prompt: "hi"})

	require.NoError(t, err)
	assert.Empty(t, output.TextCalls())
}

func TestRunnerWaitsForEndTurnAfterToolUse(t *testing.T) {
	var texts []string
	output := &mocks.OutputMock{
		TextFunc: func(text string) error {
			texts = append(texts, text)
			return nil
		},
		FinalFunc: func(stream.Result) error { return nil },
	}
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				calls++
				switch calls {
				case 1:
					return []transcript.Event{{Text: "I'll look.", SessionID: "s3", StopReason: "tool_use"}}, nil
				case 2:
					return []transcript.Event{{ToolUseIDs: []string{"t1"}, SessionID: "s3", StopReason: "tool_use"}}, nil
				case 3:
					return []transcript.Event{{ToolResultIDs: []string{"t1"}, SessionID: "s3"}}, nil
				case 4:
					return nil, nil
				case 5:
					return []transcript.Event{{Text: "final answer", SessionID: "s3", StopReason: "end_turn"}}, nil
				default:
					return nil, nil
				}
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute, IdleTimeout: time.Nanosecond,
		Prompt:     "hi",
		PollPeriod: time.Millisecond,
	})

	require.NoError(t, err)
	require.Len(t, output.FinalCalls(), 1)
	assert.Equal(t, []string{"I'll look.", "final answer"}, texts)
}

// when claude exits between typing the prompt and the new transcript being
// flushed, selectTranscript must observe session.Done() rather than poll for
// the full turn-timeout.
func TestRunnerSelectAbortsOnSessionExit(t *testing.T) {
	done := make(chan struct{})
	session := newSessionMockWithDone(done)
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			calls++
			if calls == 1 {
				close(done) // claude exits while we're still polling for transcript
			}
			return "", transcript.ErrNoTranscript
		}},
		TailerFactory: noopTailerFactory,
		Output:        &mocks.OutputMock{FinalFunc: func(stream.Result) error { return nil }},
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute,
		Prompt:     "hi",
		PollPeriod: time.Millisecond,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "claude exited")
}

// drain branch: when Claude exits but a result event is already in the
// transcript, the runner must emit a normal Final (not IsError) and return nil.
func TestRunnerSessionExitWithDrainableResult(t *testing.T) {
	done := make(chan struct{})
	session := newSessionMockWithDone(done)
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				calls++
				switch calls {
				case 1:
					return nil, nil // empty first poll
				case 2:
					close(done) // claude exits after first poll
					return nil, nil
				default:
					// drain after exit: a result event is available
					return []transcript.Event{{Text: "final answer", Result: true, SessionID: "s3"}}, nil
				}
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute,
		Prompt:     "hi",
		PollPeriod: time.Millisecond,
	})

	require.NoError(t, err, "result available in drain → normal completion, not IsError")
	require.Len(t, output.FinalCalls(), 1)
	assert.False(t, output.FinalCalls()[0].Result.IsError, "drain found result, must not flag is_error")
	assert.Equal(t, "s3", output.FinalCalls()[0].Result.SessionID)
}

// the drain after session exit must retry to absorb OS flush latency. The
// first drain returns empty, the second returns a result event — the runner
// must report success, not IsError.
func TestRunnerSessionExitDrainRetriesPickUpResult(t *testing.T) {
	done := make(chan struct{})
	session := newSessionMockWithDone(done)
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				calls++
				switch calls {
				case 1:
					return nil, nil
				case 2:
					close(done) // claude exits after first poll
					return nil, nil
				case 3:
					return nil, nil // first drain attempt: empty (OS hasn't flushed yet)
				default:
					return []transcript.Event{{Text: "answer", Result: true, SessionID: "s4"}}, nil
				}
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute,
		Prompt:     "hi",
		PollPeriod: time.Millisecond,
	})

	require.NoError(t, err, "drain retry must pick up the result event on a later attempt")
	require.Len(t, output.FinalCalls(), 1)
	assert.False(t, output.FinalCalls()[0].Result.IsError)
	assert.Equal(t, "s4", output.FinalCalls()[0].Result.SessionID)
}

// drain retry waits must honor context cancellation instead of sleeping through it.
func TestRunnerSessionExitDrainHonorsContext(t *testing.T) {
	done := make(chan struct{})
	session := newSessionMockWithDone(done)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				calls++
				switch calls {
				case 1:
					close(done) // claude exits before any result event
				case 2:
					cancel() // cancel before the drain retry wait starts
				}
				return nil, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(ctx, turn.Config{
		CWD: ".", TurnTimeout: time.Minute,
		Prompt:     "hi",
		PollPeriod: time.Hour,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "turn canceled")
	require.Len(t, output.FinalCalls(), 1)
	assert.True(t, output.FinalCalls()[0].Result.IsError)
}

func TestRunnerSessionExitDrainPropagatesError(t *testing.T) {
	done := make(chan struct{})
	session := newSessionMockWithDone(done)
	output := &mocks.OutputMock{TextFunc: func(string) error { return nil }, FinalFunc: func(stream.Result) error { return nil }}
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				calls++
				if calls == 1 {
					close(done) // claude exits before any successful poll
					return nil, nil
				}
				return nil, errors.New("tailer boom during drain")
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute,
		Prompt:     "hi",
		PollPeriod: time.Millisecond,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read transcript after session exit")
	require.Len(t, output.FinalCalls(), 1)
	assert.True(t, output.FinalCalls()[0].Result.IsError)
}

// when session.Done() fires before completion the runner must emit a Final with
// IsError:true and return an error instead of polling forever.
func TestRunnerSessionExitMidStream(t *testing.T) {
	done := make(chan struct{})
	session := newSessionMockWithDone(done)
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	calls := 0
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "x.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) {
				calls++
				if calls == 1 {
					close(done) // claude exits after first poll, before any result event
				}
				return nil, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute,
		Prompt:     "hi",
		PollPeriod: time.Millisecond,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "claude exited")
	require.Len(t, output.FinalCalls(), 1)
	assert.True(t, output.FinalCalls()[0].Result.IsError)
}

func TestRunnerValidateNilDeps(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*turn.Dependencies)
		want string
	}{
		{name: "nil starter", mod: func(d *turn.Dependencies) { d.ProcessStarter = nil }, want: "process starter is nil"},
		{name: "nil readiness", mod: func(d *turn.Dependencies) { d.Readiness = nil }, want: "readiness detector is nil"},
		{name: "nil injector", mod: func(d *turn.Dependencies) { d.Injector = nil }, want: "typing injector is nil"},
		{name: "nil catalog", mod: func(d *turn.Dependencies) { d.Catalog = nil }, want: "transcript catalog is nil"},
		{name: "nil tailers", mod: func(d *turn.Dependencies) { d.TailerFactory = nil }, want: "transcript tailer factory is nil"},
		{name: "nil output", mod: func(d *turn.Dependencies) { d.Output = nil }, want: "output writer is nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := turn.Dependencies{
				ProcessStarter: &mocks.ProcessStarterMock{},
				Readiness:      &mocks.ReadinessMock{},
				Injector:       &mocks.InjectorMock{},
				Catalog:        &mocks.CatalogMock{},
				TailerFactory:  noopTailerFactory,
				Output:         &mocks.OutputMock{},
			}
			tt.mod(&deps)

			err := turn.NewRunner(deps).Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Minute, Prompt: "x"})

			require.Error(t, err)
			assert.Equal(t, tt.want, err.Error())
		})
	}
}

func TestRunnerTimeout(t *testing.T) {
	output := &mocks.OutputMock{TextFunc: func(string) error { return nil }, FinalFunc: func(stream.Result) error { return nil }}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "session.jsonl", nil
		}},
		TailerFactory: func(string) turn.Tailer {
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) { return nil, nil }}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{CWD: ".", TurnTimeout: time.Millisecond, Prompt: "hello", PollPeriod: time.Hour})

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Len(t, output.FinalCalls(), 1)
	assert.True(t, output.FinalCalls()[0].Result.IsError)
}

func newSessionMock() *mocks.SessionMock {
	return newSessionMockWithDone(make(chan struct{}))
}

func newSessionMockWithDone(done <-chan struct{}) *mocks.SessionMock {
	return &mocks.SessionMock{
		OutputFunc: func() string { return "" },
		DoneFunc:   func() <-chan struct{} { return done },
		WriteFunc: func(p []byte) (int, error) {
			return len(p), nil
		},
		CloseFunc: func() error { return nil },
		WaitFunc:  func() error { return nil },
	}
}

func runnerWithCatalogError(err error) *turn.Runner {
	return turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return newSessionMock(), nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(context.Context, io.Writer, string) error { return nil }},
		Catalog: &mocks.CatalogMock{SelectFunc: func(string, time.Time, string) (string, error) {
			return "", err
		}},
		TailerFactory: noopTailerFactory,
		Output:        &mocks.OutputMock{FinalFunc: func(stream.Result) error { return nil }},
	})
}

func noopTailerFactory(string) turn.Tailer {
	return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, error) { return nil, nil }}
}
