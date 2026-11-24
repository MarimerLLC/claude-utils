// Package project derives a machine-independent key ("fingerprint") for each
// Claude Code project so the same repository syncs to one location regardless of
// where it is checked out on each machine.
//
// Claude names its per-project directory by dash-encoding the absolute project
// path (e.g. /Users/me/code/foo -> -Users-me-code-foo). That name differs across
// machines that check the repo out at different paths, so keying the sync repo by
// it splits one project into several. This package instead resolves the project's
// real path from Claude's session transcripts and fingerprints the git repo
// living there, falling back to the dash-encoded name for non-git directories.
package project

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxJSONLLine bounds the scanner buffer. Transcript lines embed tool output and
// can be large, so we allow well beyond bufio's 64K default.
const maxJSONLLine = 16 * 1024 * 1024

// ResolvePath recovers the real absolute project path for a Claude project
// directory (~/.claude/projects/<localHash>) by reading the "cwd" field Claude
// records in its session transcripts. The directory name itself is a lossy
// dash-encoding of the path (a dash inside a path segment is indistinguishable
// from a separator), so the transcript is the authoritative source.
//
// It scans transcripts newest-first and returns the first cwd found. ok is false
// when the directory has no transcript carrying a cwd (e.g. a stale or synthetic
// project dir), in which case callers fall back to the path key.
func ResolvePath(dir string) (path string, ok bool) {
	for _, f := range newestJSONLFirst(dir) {
		if cwd, found := scanCwd(f); found {
			return cwd, true
		}
	}
	return "", false
}

// scanCwd returns the first non-empty "cwd" value in a JSONL transcript. Lines
// are parsed as JSON (not regex-matched) so escaped paths — notably Windows
// backslashes — decode correctly.
func scanCwd(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxJSONLLine)
	needle := []byte(`"cwd"`)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, needle) {
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Cwd != "" {
			return rec.Cwd, true
		}
	}
	return "", false
}

// TranscriptStamp summarizes a project dir's transcripts (newest file name,
// its modification time, and the transcript count). When it is unchanged since
// the last index build, the project's resolved path — and so its key — can't
// have changed either, which lets Build skip the git calls. Empty when the dir
// has no transcripts.
func TranscriptStamp(dir string) string {
	items := listJSONL(dir)
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf("%s@%d#%d", filepath.Base(items[0].path), items[0].mod, len(items))
}

// transcriptRecentlyWritten reports whether dir's newest transcript was
// modified within window — i.e. Claude may still be writing its first lines, so
// a missing cwd is not yet evidence that the project is unresolvable. A dir
// with no transcripts at all (e.g. one materialized from the mirror) is not
// "pending": nothing will ever give it a cwd.
func transcriptRecentlyWritten(dir string, window time.Duration) bool {
	items := listJSONL(dir)
	return len(items) > 0 && items[0].mod >= time.Now().Add(-window).UnixNano()
}

type jsonlItem struct {
	path string
	mod  int64
}

// listJSONL lists *.jsonl transcripts in dir ordered most-recently-modified
// first (ties broken by name, for a deterministic stamp).
func listJSONL(dir string) []jsonlItem {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var items []jsonlItem
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, jsonlItem{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mod != items[j].mod {
			return items[i].mod > items[j].mod
		}
		return items[i].path < items[j].path
	})
	return items
}

// newestJSONLFirst lists *.jsonl transcripts in dir ordered most-recently-
// modified first, so the current checkout's path wins if a project has moved.
func newestJSONLFirst(dir string) []string {
	items := listJSONL(dir)
	paths := make([]string, len(items))
	for i, it := range items {
		paths[i] = it.path
	}
	return paths
}
