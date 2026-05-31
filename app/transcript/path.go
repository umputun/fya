// Package transcript discovers and tails Claude Code JSONL transcript files.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ErrNoTranscript is returned by Catalog.Select when no candidate transcript
// matches the requested since/prompt filter. Callers may retry until a fresh
// transcript appears (Claude can take a moment to flush the new file).
var ErrNoTranscript = errors.New("no matching transcript found")

// Catalog enumerates Claude Code transcript files for a given project working
// dir. Select is the only cross-package entry point.
type Catalog struct {
	root string
}

// candidate is one transcript JSONL file with its modification metadata.
type candidate struct {
	path    string
	modTime time.Time
}

// NewCatalog returns a Catalog rooted at the given Claude config dir. An empty
// root falls back to ~/.claude (or the OS-specific user home).
func NewCatalog(root string) *Catalog {
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".claude")
	}
	return &Catalog{root: root}
}

// projectDir returns the encoded project directory under root/projects for the
// supplied working directory. The cwd is resolved to an absolute path first.
func (c *Catalog) projectDir(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve cwd symlinks: %w", err)
	}
	return filepath.Join(c.root, "projects", c.encodeProjectPath(realPath)), nil
}

// candidates lists all .jsonl transcripts in the project directory for cwd,
// sorted by modification time newest-first. A missing project directory (Claude
// has not yet created it for this cwd) is returned as an empty list with no
// error so selectors can retry until the first transcript appears.
func (c *Catalog) candidates(cwd string) ([]candidate, error) {
	dir, err := c.projectDir(cwd)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read transcript dir: %w", err)
	}
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat transcript: %w", err)
		}
		candidates = append(candidates, candidate{
			path:    filepath.Join(dir, entry.Name()),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	return candidates, nil
}

// Select returns the most recent transcript in cwd that was modified at or after
// `since` and (when prompt is non-empty) contains the prompt text. Returns
// ErrNoTranscript when no candidate satisfies the filter; callers can retry as
// Claude may not have flushed the new transcript yet.
//
// Prompt matching checks multiple forms because the transcript stores the prompt
// as a JSON-encoded string: a raw search ("hello world"), the default Go
// JSON-string body, and a non-HTML-escaped JSON-string body so prompts with
// quotes, backslashes, newlines, or Ralphex markers still match.
func (c *Catalog) Select(cwd string, since time.Time, prompt string) (string, error) {
	candidates, err := c.candidates(cwd)
	if err != nil {
		return "", err
	}
	forms := c.promptForms(prompt)
	for _, candidate := range candidates {
		if candidate.modTime.Before(since) {
			continue
		}
		if prompt == "" {
			return candidate.path, nil
		}
		matches, err := c.fileContainsAny(candidate.path, forms)
		if err != nil {
			return "", fmt.Errorf("scan transcript %s: %w", candidate.path, err)
		}
		if matches {
			return candidate.path, nil
		}
	}
	return "", ErrNoTranscript
}

// promptForms returns the raw prompt plus JSON-string-encoded forms (minus
// surrounding quotes) so transcripts written with JSON escaping still match.
// Claude transcripts keep Ralphex markers as literal < and >, while json.Marshal
// HTML-escapes them, so include a non-HTML-escaped JSON form too.
// Duplicates (when the prompt has no characters needing escaping) are removed.
func (c *Catalog) promptForms(prompt string) []string {
	if prompt == "" {
		return nil
	}
	forms := []string{}
	add := func(form string) {
		if slices.Contains(forms, form) {
			return
		}
		forms = append(forms, form)
	}
	add(prompt)
	if body, ok := c.jsonStringBody(prompt); ok {
		add(body)
	}
	if body, ok := c.jsonStringBodyNoHTML(prompt); ok {
		add(body)
	}
	return forms
}

func (c *Catalog) jsonStringBody(s string) (string, bool) {
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", false
	}
	return c.trimJSONString(encoded)
}

func (c *Catalog) jsonStringBodyNoHTML(s string) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", false
	}
	return c.trimJSONString(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func (*Catalog) trimJSONString(encoded []byte) (string, bool) {
	if len(encoded) < 2 || encoded[0] != '"' || encoded[len(encoded)-1] != '"' {
		return "", false
	}
	return string(encoded[1 : len(encoded)-1]), true
}

// encodeProjectPath returns the Claude Code project directory encoding of path:
// every rune that is not a letter or digit becomes '-'. Matches Claude Code's
// internal encoding so cwd "/Users/x/repo" → "-Users-x-repo".
func (*Catalog) encodeProjectPath(path string) string {
	var b strings.Builder
	for _, r := range path {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return b.String()
}

// fileContainsAny streams path looking for any of the needles and returns true
// on the first match. The overlap window between chunk reads is sized to the
// longest needle so a match split across a read boundary is not missed. Read
// errors are surfaced rather than silently treated as "no match" — permission
// errors or partial reads otherwise hide real problems.
func (*Catalog) fileContainsAny(path string, needles []string) (bool, error) {
	if len(needles) == 0 {
		return false, nil
	}
	f, err := os.Open(path) //nolint:gosec // path is built by Catalog from a controlled ProjectDir.
	if err != nil {
		return false, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	needleBytes := make([][]byte, len(needles))
	maxLen := 0
	for i, n := range needles {
		needleBytes[i] = []byte(n)
		if len(needleBytes[i]) > maxLen {
			maxLen = len(needleBytes[i])
		}
	}
	overlap := maxLen
	reader := bufio.NewReaderSize(f, 64*1024)
	tail := make([]byte, 0, overlap)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			scan := append(tail, buf[:n]...) //nolint:gocritic // overlap window across chunks.
			for _, needle := range needleBytes {
				if bytes.Contains(scan, needle) {
					return true, nil
				}
			}
			if len(scan) > overlap {
				tail = append(tail[:0], scan[len(scan)-overlap:]...)
			} else {
				tail = append(tail[:0], scan...)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
		if readErr != nil {
			return false, fmt.Errorf("read transcript: %w", readErr)
		}
	}
}
