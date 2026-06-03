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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				return []transcript.Event{{Text: "answer", Result: true, SessionID: "s1"}}, true, nil
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				return []transcript.Event{{Result: true, SessionID: "s"}}, true, nil
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				return nil, false, errors.New("tail boom")
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				return []transcript.Event{{Text: "x", Result: true, SessionID: "s"}}, true, nil
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				if calls == 1 {
					return []transcript.Event{{Text: "answer", SessionID: "s2"}}, true, nil
				}
				return nil, false, nil
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

func TestRunnerTranscriptActivityDelaysIdleCompletion(t *testing.T) {
	output := &mocks.OutputMock{
		TextFunc:  func(string) error { return nil },
		FinalFunc: func(stream.Result) error { return nil },
	}
	calls := 0
	finalAt := 0
	activityPolls := 8
	output.FinalFunc = func(stream.Result) error {
		finalAt = calls
		return nil
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				switch {
				case calls == 1:
					return []transcript.Event{{Text: "answer", SessionID: "s2"}}, true, nil
				case calls <= activityPolls+1:
					return nil, true, nil
				default:
					return nil, false, nil
				}
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Second, IdleTimeout: 20 * time.Millisecond,
		Prompt:     "hi",
		PollPeriod: 5 * time.Millisecond,
	})

	require.NoError(t, err)
	require.Len(t, output.FinalCalls(), 1)
	assert.Greater(t, finalAt, activityPolls+1)
}

func TestRunnerEmitsDeltaTextWithoutStreamMessage(t *testing.T) {
	var texts []string
	var events []stream.Event
	output := &mocks.OutputMock{
		EventFunc: func(event stream.Event) error {
			events = append(events, event)
			return nil
		},
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				return []transcript.Event{{Type: "assistant", Text: "streamed chunk", Result: true, SessionID: "s"}}, true, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute, IdleTimeout: time.Second, StreamEvents: true,
		Prompt: "hi",
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"streamed chunk"}, texts)
	assert.Empty(t, events)
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				return []transcript.Event{{
					Type:      "assistant",
					Text:      "answer",
					Message:   []byte(`{"role":"assistant","content":[{"type":"text","text":"answer"}]}`),
					Result:    true,
					SessionID: "s",
				}}, true, nil
			}}
		},
		Output: output,
	})

	err := runner.Run(t.Context(), turn.Config{
		CWD: ".", TurnTimeout: time.Minute, IdleTimeout: time.Second, StreamEvents: true,
		Prompt: "hi",
	})

	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "assistant", events[0].Type)
	assert.Equal(t, "s", events[0].SessionID)
	assert.Empty(t, output.TextCalls(), "raw stream events replace legacy text deltas")
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				switch calls {
				case 1:
					return []transcript.Event{{Text: "I'll look.", SessionID: "s3", StopReason: "tool_use"}}, true, nil
				case 2:
					return []transcript.Event{{ToolUseIDs: []string{"t1"}, SessionID: "s3", StopReason: "tool_use"}}, true, nil
				case 3:
					return []transcript.Event{{ToolResultIDs: []string{"t1"}, SessionID: "s3"}}, true, nil
				case 4:
					return nil, false, nil
				case 5:
					return []transcript.Event{{Text: "final answer", SessionID: "s3", StopReason: "end_turn"}}, true, nil
				default:
					return nil, false, nil
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

func TestRunnerCompletesPostToolAnswerWithoutEndTurn(t *testing.T) {
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				switch calls {
				case 1:
					return []transcript.Event{{Text: "I'll look.", SessionID: "s3", StopReason: "tool_use"}}, true, nil
				case 2:
					return []transcript.Event{{ToolUseIDs: []string{"t1"}, SessionID: "s3", StopReason: "tool_use"}}, true, nil
				case 3:
					return []transcript.Event{{ToolResultIDs: []string{"t1"}, SessionID: "s3"}}, true, nil
				case 4:
					return nil, false, nil
				case 5:
					return []transcript.Event{{Text: "final verdict", SessionID: "s3"}}, true, nil
				default:
					return nil, false, nil
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
	assert.Equal(t, []string{"I'll look.", "final verdict"}, texts)
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				switch calls {
				case 1:
					return nil, false, nil // empty first poll
				case 2:
					close(done) // claude exits after first poll
					return nil, false, nil
				default:
					// drain after exit: a result event is available
					return []transcript.Event{{Text: "final answer", Result: true, SessionID: "s3"}}, true, nil
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				switch calls {
				case 1:
					return nil, false, nil
				case 2:
					close(done) // claude exits after first poll
					return nil, false, nil
				case 3:
					return nil, false, nil // first drain attempt: empty (OS hasn't flushed yet)
				default:
					return []transcript.Event{{Text: "answer", Result: true, SessionID: "s4"}}, true, nil
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

type sessionExitDrainTest struct {
	readNew func(call int, done chan struct{}) ([]transcript.Event, bool, error)
	config  turn.Config
}

type sessionExitDrainResult struct {
	err    error
	output *mocks.OutputMock
	texts  []string
}

func runSessionExitDrainTest(t *testing.T, tc sessionExitDrainTest) sessionExitDrainResult {
	t.Helper()
	done := make(chan struct{})
	session := newSessionMockWithDone(done)
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				return tc.readNew(calls, done)
			}}
		},
		Output: output,
	})

	cfg := tc.config
	cfg.CWD = "."
	cfg.Prompt = "hi"
	err := runner.Run(t.Context(), cfg)

	return sessionExitDrainResult{err: err, output: output, texts: texts}
}

func TestRunnerSessionExitDrainCompletesFinalAssistantText(t *testing.T) {
	res := runSessionExitDrainTest(t, sessionExitDrainTest{
		config: turn.Config{TurnTimeout: time.Second, IdleTimeout: time.Second, PollPeriod: time.Millisecond},
		readNew: func(call int, done chan struct{}) ([]transcript.Event, bool, error) {
			switch call {
			case 1:
				close(done)
				return nil, false, nil
			case 2:
				return []transcript.Event{{Text: "final answer", SessionID: "s5"}}, true, nil
			default:
				return nil, false, nil
			}
		},
	})

	require.NoError(t, res.err)
	require.Len(t, res.output.FinalCalls(), 1)
	assert.False(t, res.output.FinalCalls()[0].Result.IsError)
	assert.Equal(t, "s5", res.output.FinalCalls()[0].Result.SessionID)
	assert.Equal(t, []string{"final answer"}, res.texts)
}

func TestRunnerSessionExitDrainCompletesPostToolFinalAssistantText(t *testing.T) {
	res := runSessionExitDrainTest(t, sessionExitDrainTest{
		config: turn.Config{TurnTimeout: 5 * time.Second, IdleTimeout: time.Second, PollPeriod: 10 * time.Millisecond},
		readNew: func(call int, done chan struct{}) ([]transcript.Event, bool, error) {
			switch call {
			case 1:
				return []transcript.Event{{
					Text: "I'll check.", ToolUseIDs: []string{"t1"}, StopReason: "tool_use", SessionID: "s6",
				}}, true, nil
			case 2:
				close(done)
				return []transcript.Event{{ToolResultIDs: []string{"t1"}, SessionID: "s6"}}, true, nil
			case 3:
				return []transcript.Event{{Text: "final verdict", SessionID: "s6"}}, true, nil
			default:
				return nil, false, nil
			}
		},
	})

	require.NoError(t, res.err)
	require.Len(t, res.output.FinalCalls(), 1)
	assert.False(t, res.output.FinalCalls()[0].Result.IsError)
	assert.Equal(t, "s6", res.output.FinalCalls()[0].Result.SessionID)
	assert.Equal(t, []string{"I'll check.", "final verdict"}, res.texts)
}

func TestRunnerSessionExitDrainToolResultAloneDoesNotComplete(t *testing.T) {
	res := runSessionExitDrainTest(t, sessionExitDrainTest{
		config: turn.Config{TurnTimeout: time.Second, IdleTimeout: time.Second, PollPeriod: time.Hour},
		readNew: func(call int, done chan struct{}) ([]transcript.Event, bool, error) {
			switch call {
			case 1:
				close(done)
				return []transcript.Event{{
					Text: "I'll check.", ToolUseIDs: []string{"t1"}, StopReason: "tool_use", SessionID: "s7",
				}}, true, nil
			case 2:
				return []transcript.Event{{ToolResultIDs: []string{"t1"}, SessionID: "s7"}}, true, nil
			default:
				return nil, false, nil
			}
		},
	})

	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "claude exited before turn completion")
	require.Len(t, res.output.FinalCalls(), 1)
	assert.True(t, res.output.FinalCalls()[0].Result.IsError)
	assert.Equal(t, "s7", res.output.FinalCalls()[0].Result.SessionID)
	assert.Equal(t, []string{"I'll check."}, res.texts)
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				switch calls {
				case 1:
					close(done) // claude exits before any result event
				case 2:
					cancel() // cancel before the drain retry wait starts
				}
				return nil, false, nil
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				if calls == 1 {
					close(done) // claude exits before any successful poll
					return nil, false, nil
				}
				return nil, false, errors.New("tailer boom during drain")
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) {
				calls++
				if calls == 1 {
					close(done) // claude exits after first poll, before any result event
				}
				return nil, false, nil
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

func TestRunnerStartTimeout(t *testing.T) {
	output := &mocks.OutputMock{FinalFunc: func(stream.Result) error { return nil }}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(ctx context.Context, _ ptyrun.Config) (turn.Session, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		Readiness:     &mocks.ReadinessMock{},
		Injector:      &mocks.InjectorMock{},
		Catalog:       &mocks.CatalogMock{},
		TailerFactory: noopTailerFactory,
		Output:        output,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := runner.Run(ctx, turn.Config{CWD: ".", TurnTimeout: time.Millisecond, Prompt: "hello"})

	assert.Contains(t, err.Error(), "start claude pty")
	assertTurnTimeoutFinal(t, err, output)
}

func TestRunnerReadinessTimeout(t *testing.T) {
	session := newSessionMock()
	output := &mocks.OutputMock{FinalFunc: func(stream.Result) error { return nil }}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(ctx context.Context, _ ready.Source) (ready.Result, error) {
			<-ctx.Done()
			return ready.Result{}, ctx.Err()
		}},
		Injector:      &mocks.InjectorMock{},
		Catalog:       &mocks.CatalogMock{},
		TailerFactory: noopTailerFactory,
		Output:        output,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := runner.Run(ctx, turn.Config{CWD: ".", TurnTimeout: time.Millisecond, Prompt: "hello"})

	assert.Contains(t, err.Error(), "wait claude readiness")
	assertTurnTimeoutFinal(t, err, output)
	assert.Len(t, session.CloseCalls(), 1)
	assert.Len(t, session.WaitCalls(), 1)
}

func TestRunnerInjectTimeout(t *testing.T) {
	session := newSessionMock()
	output := &mocks.OutputMock{FinalFunc: func(stream.Result) error { return nil }}
	runner := turn.NewRunner(turn.Dependencies{
		ProcessStarter: &mocks.ProcessStarterMock{StartFunc: func(context.Context, ptyrun.Config) (turn.Session, error) {
			return session, nil
		}},
		Readiness: &mocks.ReadinessMock{WaitFunc: func(context.Context, ready.Source) (ready.Result, error) {
			return ready.Result{Ready: true}, nil
		}},
		Injector: &mocks.InjectorMock{TypeFunc: func(ctx context.Context, _ io.Writer, _ string) error {
			<-ctx.Done()
			return fmt.Errorf("type canceled: %w", ctx.Err())
		}},
		Catalog:       &mocks.CatalogMock{},
		TailerFactory: noopTailerFactory,
		Output:        output,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := runner.Run(ctx, turn.Config{CWD: ".", TurnTimeout: time.Millisecond, Prompt: "hello"})

	assert.Contains(t, err.Error(), "type prompt")
	assertTurnTimeoutFinal(t, err, output)
	assert.Len(t, session.CloseCalls(), 1)
	assert.Len(t, session.WaitCalls(), 1)
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
			return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) { return nil, false, nil }}
		},
		Output: output,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := runner.Run(ctx, turn.Config{CWD: ".", TurnTimeout: time.Millisecond, Prompt: "hello", PollPeriod: time.Hour})

	assertTurnTimeoutFinal(t, err, output)
}

func assertTurnTimeoutFinal(t *testing.T, err error, output *mocks.OutputMock) {
	t.Helper()
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, turn.ErrTurnTimeout)
	require.Len(t, output.FinalCalls(), 1)
	assert.True(t, output.FinalCalls()[0].Result.IsError)
	assert.Equal(t, "fya_turn_timeout", output.FinalCalls()[0].Result.TerminalReason)
	assert.Contains(t, output.FinalCalls()[0].Result.Result, "FYA_TRANSIENT_TIMEOUT")
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
	return &mocks.TailerMock{ReadNewFunc: func() ([]transcript.Event, bool, error) { return nil, false, nil }}
}
