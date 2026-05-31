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
	path   string
	offset int64
	parser *parser
}

// NewTailer returns a Tailer pointed at path.
func NewTailer(path string) *Tailer {
	return &Tailer{path: path, parser: &parser{}}
}

// ReadNew opens the transcript file, reads from the current offset to EOF, and
// returns any complete lines parsed as Events. Partial trailing bytes are left
// unread; offset is only advanced past lines terminated by '\n' so a torn write
// at EOF will be picked up cleanly on the next poll.
func (t *Tailer) ReadNew() ([]Event, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek transcript: %w", err)
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
				return nil, parseErr
			}
			events = append(events, event)
		}
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read transcript: %w", err)
		}
	}
}

// Completion encapsulates the rule for deciding when a turn is done.
type Completion struct {
	IdleTimeout time.Duration
}

// Done returns true when the turn should terminate: either the current event is
// a terminal transcript record, or the tracker has seen assistant activity, no
// tool_use IDs remain pending, no tool_use stop_reason is waiting for a later
// end_turn, and the idle window has elapsed.
func (c Completion) Done(tracker *Tracker, event Event, idleFor time.Duration) bool {
	if event.Result {
		return true
	}
	if tracker == nil || tracker.pendingCount() > 0 || !tracker.canIdleComplete() {
		return false
	}
	return c.IdleTimeout > 0 && idleFor >= c.IdleTimeout
}
