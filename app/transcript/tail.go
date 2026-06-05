package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Tailer reads new JSONL records from a transcript file, advancing offset only
// past complete newline-terminated lines so a poll that catches Claude mid-write
// can re-read the partial line on the next call.
type Tailer struct {
	path     string
	offset   int64
	size     int64
	seenSize bool
	parser   *parser
}

// NewTailer returns a Tailer pointed at path.
func NewTailer(path string) *Tailer {
	return &Tailer{path: path, parser: &parser{}}
}

// ReadNew opens the transcript file, reads from the current offset to EOF, and
// returns complete lines parsed as Events plus a file-activity flag. Partial
// trailing bytes are left unread; offset is only advanced past lines terminated
// by '\n' so a torn write at EOF will be picked up cleanly on the next poll.
func (t *Tailer) ReadNew() ([]Event, bool, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, false, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat transcript: %w", err)
	}
	activity := !t.seenSize || info.Size() != t.size
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, activity, fmt.Errorf("seek transcript: %w", err)
	}
	reader := bufio.NewReader(f)
	events := []Event{}
	for {
		line, err := reader.ReadBytes('\n')
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		if complete {
			t.offset += int64(len(line))
			event, parseErr := t.parser.parse(bytes.TrimRight(line, "\r\n"))
			if parseErr != nil {
				return nil, activity, parseErr
			}
			events = append(events, event)
		}
		if errors.Is(err, io.EOF) {
			info, statErr := f.Stat()
			if statErr != nil {
				return nil, false, fmt.Errorf("stat transcript: %w", statErr)
			}
			activity = activity || !t.seenSize || info.Size() != t.size || len(events) > 0
			t.seenSize = true
			t.size = info.Size()
			return events, activity, nil
		}
		if err != nil {
			return nil, activity, fmt.Errorf("read transcript: %w", err)
		}
	}
}

// Completion encapsulates the rule for deciding when a turn is done.
type Completion struct {
	IdleTimeout time.Duration
}

// Done returns true when the turn should terminate: either the current event is
// a terminal transcript record, or the tracker has seen completion-eligible
// assistant text, no tool_use IDs remain pending, no tool turn is waiting for a
// later assistant answer, and the idle window has elapsed.
func (c Completion) Done(tracker *Tracker, event Event, idleFor time.Duration) bool {
	if event.Result {
		return true
	}
	if tracker == nil || tracker.pendingCount() > 0 || !tracker.canIdleComplete() {
		return false
	}
	return c.IdleTimeout > 0 && idleFor >= c.IdleTimeout
}

// Eligible reports whether the turn could complete on idle: a terminal record,
// or completion-eligible assistant text with no pending tool work. Unlike Done
// it ignores the idle window, so callers can tell "would complete once quiet"
// apart from "has been quiet long enough".
func (c Completion) Eligible(tracker *Tracker, event Event) bool {
	if event.Result {
		return true
	}
	return tracker != nil && tracker.pendingCount() == 0 && tracker.canIdleComplete()
}
