package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolvePath_ExtractsCwd(t *testing.T) {
	dir := t.TempDir()
	line := `{"parentUuid":null,"type":"user","cwd":"/Users/mattartist/Code/repos/claude-utils","message":{"role":"user"}}`
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok := ResolvePath(dir)
	if !ok || got != "/Users/mattartist/Code/repos/claude-utils" {
		t.Fatalf("ResolvePath = %q, %v; want the cwd", got, ok)
	}
}

func TestResolvePath_WindowsPathUnescaped(t *testing.T) {
	dir := t.TempDir()
	// Backslashes are JSON-escaped in the transcript; JSON decoding must restore them.
	line := `{"type":"user","cwd":"C:\\Users\\rocky\\src\\app"}`
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok := ResolvePath(dir)
	if !ok || got != `C:\Users\rocky\src\app` {
		t.Fatalf("ResolvePath = %q, %v; want unescaped Windows path", got, ok)
	}
}

func TestResolvePath_SkipsLinesWithoutCwd(t *testing.T) {
	dir := t.TempDir()
	content := `{"type":"mode","mode":"normal"}
{"type":"permission-mode","permissionMode":"plan"}
{"type":"user","cwd":"/home/matta/repos/foo"}
`
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok := ResolvePath(dir)
	if !ok || got != "/home/matta/repos/foo" {
		t.Fatalf("ResolvePath = %q, %v; want the cwd from a later line", got, ok)
	}
}

func TestResolvePath_NewestTranscriptWins(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.jsonl")
	newer := filepath.Join(dir, "new.jsonl")
	if err := os.WriteFile(old, []byte(`{"cwd":"/old/path"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte(`{"cwd":"/new/path"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Make "old" genuinely older so ordering is deterministic.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	got, ok := ResolvePath(dir)
	if !ok || got != "/new/path" {
		t.Fatalf("ResolvePath = %q, %v; want the newest transcript's cwd", got, ok)
	}
}

func TestResolvePath_NoTranscript(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ResolvePath(dir); ok {
		t.Errorf("expected ok=false for a dir with no transcript")
	}
}
