package transcript

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogEncodeProjectPath(t *testing.T) {
	cat := Catalog{}

	assert.Equal(t, "-Users-me-dev-fya", cat.encodeProjectPath("/Users/me/dev/fya"))
}

func TestCatalogProjectDirResolvesSymlinkedCWD(t *testing.T) {
	root := t.TempDir()
	realCWD := t.TempDir()
	linkCWD := filepath.Join(t.TempDir(), "cwd-link")
	require.NoError(t, os.Symlink(realCWD, linkCWD))
	cat := NewCatalog(root)

	got, err := cat.projectDir(linkCWD)

	require.NoError(t, err)
	physicalCWD, err := filepath.EvalSymlinks(realCWD)
	require.NoError(t, err)
	want := filepath.Join(root, "projects", cat.encodeProjectPath(physicalCWD))
	assert.Equal(t, want, got)
}

func TestCatalogSelectSymlinkedCWD(t *testing.T) {
	root := t.TempDir()
	realCWD := t.TempDir()
	linkCWD := filepath.Join(t.TempDir(), "cwd-link")
	require.NoError(t, os.Symlink(realCWD, linkCWD))
	cat := NewCatalog(root)
	physicalCWD, err := filepath.EvalSymlinks(realCWD)
	require.NoError(t, err)
	dir := filepath.Join(root, "projects", cat.encodeProjectPath(physicalCWD))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	transcriptPath := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte("target prompt"), 0o600))

	got, err := cat.Select(linkCWD, time.Now().Add(-time.Minute), "target prompt")

	require.NoError(t, err)
	assert.Equal(t, transcriptPath, got)
}

func TestCatalogSelect(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	cat := NewCatalog(root)
	dir, err := cat.projectDir(cwd)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	require.NoError(t, os.WriteFile(oldPath, []byte("old prompt"), 0o600))
	require.NoError(t, os.WriteFile(newPath, []byte("target prompt"), 0o600))
	now := time.Now()
	require.NoError(t, os.Chtimes(oldPath, now.Add(-time.Hour), now.Add(-time.Hour)))
	require.NoError(t, os.Chtimes(newPath, now, now))

	got, err := cat.Select(cwd, now.Add(-time.Minute), "target prompt")

	require.NoError(t, err)
	assert.Equal(t, newPath, got)
}

// when no transcript is fresh enough or matches the prompt, Select must NOT
// silently return a stale candidate — the caller relies on ErrNoTranscript so it
// can wait for Claude to create the new transcript.
func TestCatalogSelectNoStaleFallback(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	cat := NewCatalog(root)
	dir, err := cat.projectDir(cwd)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	stalePath := filepath.Join(dir, "stale.jsonl")
	require.NoError(t, os.WriteFile(stalePath, []byte("anything"), 0o600))
	now := time.Now()
	require.NoError(t, os.Chtimes(stalePath, now.Add(-time.Hour), now.Add(-time.Hour)))

	_, err = cat.Select(cwd, now.Add(-time.Minute), "fresh prompt")

	assert.ErrorIs(t, err, ErrNoTranscript)
}

// when Claude hasn't created the project transcript dir yet (first run in a
// new cwd), os.ReadDir returns fs.ErrNotExist. The runner relies on this
// surfacing as ErrNoTranscript so its poll loop can wait instead of failing.
func TestCatalogSelectMissingProjectDirIsErrNoTranscript(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	cat := NewCatalog(root)
	// note: no os.MkdirAll for the project dir.

	_, err := cat.Select(cwd, time.Now().Add(-time.Minute), "anything")

	assert.ErrorIs(t, err, ErrNoTranscript)
}

// prompts containing JSON-escapable characters (newlines, quotes, backslashes)
// must still match — Claude stores them as JSON-encoded strings in the
// transcript, so a raw search would miss valid candidates.
func TestCatalogSelectJSONEscapedPrompt(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	cat := NewCatalog(root)
	dir, err := cat.projectDir(cwd)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	// transcript line as Claude would write it: prompt body JSON-escaped.
	transcriptPath := filepath.Join(dir, "session.jsonl")
	body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"line1\nline2 \"quoted\""}]}}`
	require.NoError(t, os.WriteFile(transcriptPath, []byte(body+"\n"), 0o600))

	got, err := cat.Select(cwd, time.Now().Add(-time.Minute), "line1\nline2 \"quoted\"")

	require.NoError(t, err)
	assert.Equal(t, transcriptPath, got)
}

func TestCatalogSelectRalphexSignalPrompt(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	cat := NewCatalog(root)
	dir, err := cat.projectDir(cwd)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	transcriptPath := filepath.Join(dir, "session.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"line1\nOutput exactly: <<<RALPHEX:ALL_TASKS_DONE>>>"}}`
	require.NoError(t, os.WriteFile(transcriptPath, []byte(body+"\n"), 0o600))

	got, err := cat.Select(cwd, time.Now().Add(-time.Minute), "line1\nOutput exactly: <<<RALPHEX:ALL_TASKS_DONE>>>")

	require.NoError(t, err)
	assert.Equal(t, transcriptPath, got)
}

func TestCatalogSelectFreshButMissingPromptReturnsErrNoTranscript(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	cat := NewCatalog(root)
	dir, err := cat.projectDir(cwd)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	freshPath := filepath.Join(dir, "fresh.jsonl")
	require.NoError(t, os.WriteFile(freshPath, []byte("different content"), 0o600))

	_, err = cat.Select(cwd, time.Now().Add(-time.Minute), "specific prompt")

	assert.ErrorIs(t, err, ErrNoTranscript)
}
