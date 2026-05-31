package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/fya/app/options"
	"github.com/umputun/fya/app/stream"
)

func TestRalphexContractFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/ralphex/basic_stream.jsonl")
	require.NoError(t, err)

	result, err := parseRalphexOutput(string(data))

	require.NoError(t, err)
	assert.Equal(t, "fixed the task\n<<<RALPHEX:ALL_TASKS_DONE>>>", result)
}

func TestStreamOutputAcceptedByRalphexContract(t *testing.T) {
	var out bytes.Buffer
	writer := stream.NewWriter(&out, stream.Config{Format: stream.FormatStreamJSON, SessionID: "s1"})

	require.NoError(t, writer.Text("fixed "))
	require.NoError(t, writer.Text("the task\n"))
	require.NoError(t, writer.Final(stream.Result{}))

	got, err := parseRalphexOutput(out.String())
	require.NoError(t, err)
	assert.Equal(t, "fixed the task\n", got)
	assert.Equal(t, "result", lastEventType(t, out.String()))
}

func TestRalphexSignalPassthrough(t *testing.T) {
	const signal = "<<<RALPHEX:ALL_TASKS_DONE>>>"
	var out bytes.Buffer
	writer := stream.NewWriter(&out, stream.Config{Format: stream.FormatStreamJSON})

	require.NoError(t, writer.Text("done "+signal))
	require.NoError(t, writer.Final(stream.Result{}))

	event := eventContainingText(t, out.String(), signal)
	assert.Equal(t, "assistant", event.Type)
	assert.Equal(t, "done "+signal, event.messageText())
}

func TestStreamFinalDoesNotDuplicateRalphexOutput(t *testing.T) {
	var out bytes.Buffer
	writer := stream.NewWriter(&out, stream.Config{Format: stream.FormatStreamJSON})

	require.NoError(t, writer.Text("already streamed"))
	require.NoError(t, writer.Final(stream.Result{}))

	got, err := parseRalphexOutput(out.String())
	require.NoError(t, err)
	assert.Equal(t, "already streamed", got)
}

func TestRalphexCommandShapeOptions(t *testing.T) {
	args := []string{
		"--dangerously-skip-permissions",
		"--output-format", "stream-json",
		"--verbose",
		"--model", "opus",
		"--effort", "high",
		"--print",
	}

	cfg, err := options.NewParser().Parse(args)

	require.NoError(t, err)
	assert.Equal(t, stream.FormatStreamJSON, cfg.OutputFormat)
	wantClaudeArgs := []string{"--dangerously-skip-permissions", "--verbose", "--model", "opus", "--effort", "high"}
	assert.Equal(t, wantClaudeArgs, cfg.ClaudeArgs)
}

type ralphexEvent struct {
	Type    string          `json:"type"`
	Result  json.RawMessage `json:"result"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

func (e ralphexEvent) messageText() string {
	var out strings.Builder
	for _, block := range e.Message.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String()
}

func parseRalphexOutput(data string) (string, error) {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // allow lines up to 1 MiB so generated outputs are not silently truncated.
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		event := ralphexEvent{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		switch event.Type {
		case "assistant":
			out.WriteString(event.messageText())
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				out.WriteString(event.Delta.Text)
			}
		case "result":
			if len(event.Result) == 0 {
				continue
			}
			var resultString string
			if err := json.Unmarshal(event.Result, &resultString); err == nil {
				continue
			}
			var resultObject struct {
				Output string `json:"output"`
			}
			if err := json.Unmarshal(event.Result, &resultObject); err == nil {
				out.WriteString(resultObject.Output)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan ralphex output: %w", err)
	}
	return out.String(), nil
}

func lastEventType(t *testing.T, data string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(data), "\n")
	require.NotEmpty(t, lines, "no event lines")
	return decodeEvent(t, lines[len(lines)-1]).Type
}

func eventContainingText(t *testing.T, data, needle string) ralphexEvent {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		event := decodeEvent(t, scanner.Text())
		if strings.Contains(event.Delta.Text, needle) || strings.Contains(event.messageText(), needle) {
			return event
		}
	}
	require.NoError(t, scanner.Err())
	t.Fatalf("no event contains %q in %q", needle, data)
	return ralphexEvent{}
}

func decodeEvent(t *testing.T, line string) ralphexEvent {
	t.Helper()
	event := ralphexEvent{}
	require.NoError(t, json.Unmarshal([]byte(line), &event))
	return event
}
