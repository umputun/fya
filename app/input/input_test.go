package input

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadText(t *testing.T) {
	tests := []struct {
		name string
		args []string
		in   string
		has  bool
		want string
	}{
		{name: "args win over stdin", args: []string{"from", "args"}, in: "from stdin\n", has: true, want: "from args"},
		{name: "stdin without trailing newline", in: "from stdin", has: true, want: "from stdin"},
		{name: "args fallback", args: []string{"from", "args"}, has: false, want: "from args"},
		{name: "empty stdin falls back", args: []string{"from", "args"}, has: true, want: "from args"},
		{name: "trims crlf", in: "hello\r\n", has: true, want: "hello"},
		{name: "normalizes internal crlf", in: "line1\r\nline2\n", has: true, want: "line1\nline2"},
		{name: "normalizes lone cr", in: "a\rb", has: true, want: "a\nb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewReader(Request{Args: tt.args, Stdin: strings.NewReader(tt.in), StdinHasData: tt.has, InputFormat: "text"}).Read()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadTextEmptyPrompt(t *testing.T) {
	_, err := NewReader(Request{Stdin: strings.NewReader(" \n"), StdinHasData: true, InputFormat: "text"}).Read()

	assert.ErrorIs(t, err, ErrEmptyPrompt)
}

func TestReadTextArgsSkipStdin(t *testing.T) {
	got, err := NewReader(Request{Args: []string{"from", "args"}, Stdin: errReader{}, StdinHasData: true, InputFormat: "text"}).Read()

	require.NoError(t, err)
	assert.Equal(t, "from args", got)
}

func TestReadTextReadError(t *testing.T) {
	_, err := NewReader(Request{Stdin: errReader{}, StdinHasData: true, InputFormat: "text"}).Read()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "read prompt")
}

func TestReadStreamJSON(t *testing.T) {
	in := `{"type":"system","message":"ready"}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}}`

	got, err := NewReader(Request{Stdin: strings.NewReader(in), StdinHasData: true, InputFormat: "stream-json"}).Read()

	require.NoError(t, err)
	assert.Equal(t, "hello world", got)
}

func TestReadStreamJSONNormalizesNewlines(t *testing.T) {
	in := `{"type":"user","message":{"role":"user","content":"a\r\nb\rc"}}`

	got, err := NewReader(Request{Stdin: strings.NewReader(in), StdinHasData: true, InputFormat: "stream-json"}).Read()

	require.NoError(t, err)
	assert.Equal(t, "a\nb\nc", got, "internal CRLF and lone CR normalized to LF so no bare CR submits early")
}

func TestReadStreamJSONContentForms(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "top content string", in: `{"type":"user","content":"hello"}`, want: "hello"},
		{name: "message content string", in: `{"type":"user","message":{"content":"hello"}}`, want: "hello"},
		{name: "role only", in: `{"message":{"role":"user","content":[{"text":"hello"}]}}`, want: "hello"},
		{name: "nested content", in: `{"type":"user","content":[{"content":"hello"}]}`, want: "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewReader(Request{Stdin: strings.NewReader(tt.in), StdinHasData: true, InputFormat: "stream-json"}).Read()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadStreamJSONRejectsMultipleUsers(t *testing.T) {
	in := `{"type":"user","content":"one"}` + "\n" + `{"type":"user","content":"two"}`

	_, err := NewReader(Request{Stdin: strings.NewReader(in), StdinHasData: true, InputFormat: "stream-json"}).Read()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one user message")
}

func TestReadStreamJSONReplayUserMessages(t *testing.T) {
	const raw = `{"type":"user","content":"hello"}`
	var out bytes.Buffer

	got, err := NewReader(Request{
		Stdin:              strings.NewReader(raw + "\n"),
		StdinHasData:       true,
		Stdout:             &out,
		InputFormat:        "stream-json",
		ReplayUserMessages: true,
	}).Read()

	require.NoError(t, err)
	assert.Equal(t, "hello", got)
	assert.Equal(t, raw+"\n", out.String()) //nolint:testifylint // need byte-exact replay including trailing newline, not JSON-equivalent.
}

func TestReadStreamJSONErrors(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want string
	}{
		{name: "requires stdin", req: Request{InputFormat: "stream-json"}, want: "requires stdin"},
		{name: "invalid json", req: Request{Stdin: strings.NewReader("{"), StdinHasData: true, InputFormat: "stream-json"}, want: "parse stream-json"},
		{name: "no user", req: Request{Stdin: strings.NewReader(`{"type":"system"}`), StdinHasData: true, InputFormat: "stream-json"}, want: "prompt is required"},
		{name: "empty user", req: Request{Stdin: strings.NewReader(`{"type":"user","content":" "}`), StdinHasData: true, InputFormat: "stream-json"}, want: "prompt is required"},
		{name: "replay missing stdout", req: Request{Stdin: strings.NewReader(`{"type":"user","content":"hello"}`), StdinHasData: true, InputFormat: "stream-json", ReplayUserMessages: true}, want: "requires stdout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewReader(tt.req).Read()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) {
	return 0, errors.New("read failed")
}
