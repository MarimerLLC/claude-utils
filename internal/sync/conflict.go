package sync

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/merge"
)

// Resolution describes how resolveConflicts settled a stopped merge.
type Resolution struct {
	Files []ResolvedFile
	// Backups are the losing versions to keep as .from-remote-* copies. They
	// are not written by resolveConflicts: the caller writes them (see
	// WriteBackups) only once the merge is committed, so an aborted merge
	// that is retried every tick leaves nothing behind.
	Backups []Backup
}

// Backup is one losing version to save next to its file.
type Backup struct {
	Path    string // repo-relative, slash-separated, including the .from-remote-* suffix
	Content []byte
}

// WriteBackups writes the backups under the work-tree at dir.
func (r Resolution) WriteBackups(dir string) error {
	for _, b := range r.Backups {
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(b.Path)), b.Content); err != nil {
			return err
		}
	}
	return nil
}

// literal turns a repo-relative path into a pathspec that matches only that
// path: without it, *, ? and [...] in a file name act as glob characters,
// and `git rm --cached` could drop unrelated files from the merge.
func literal(p string) string { return ":(literal)" + p }

// ResolvedFile is one conflicted path and how it was settled.
type ResolvedFile struct {
	Path string // repo-relative, slash-separated
	How  string // e.g. "MEMORY.md merge", "merged", "kept ours", "kept theirs"
}

func (r Resolution) String() string {
	parts := make([]string, len(r.Files))
	for i, f := range r.Files {
		parts[i] = fmt.Sprintf("%s (%s)", f.Path, f.How)
	}
	s := strings.Join(parts, ", ")
	if len(r.Backups) > 0 {
		names := make([]string, len(r.Backups))
		for i, b := range r.Backups {
			names[i] = b.Path
		}
		s += "; backups: " + strings.Join(names, ", ")
	}
	return s
}

// resolveConflicts settles every conflicted path of a merge that stopped
// mid-pull and stages the results, so the caller can commit the merge. In a
// merge, "ours" (stage 2) is this PC's HEAD and "theirs" (stage 3) is origin.
//
//   - MEMORY.md: merged section by section in-process (merge.Merge), which
//     keeps both versions of overlapping edits, so it never leaves markers and
//     doesn't depend on the merge driver being configured.
//   - Any other file both sides changed (or both added): see resolveText. For
//     a file both sides added, git has no base; the newest version both
//     sides' histories share stands in (see sharedVersion). The losing
//     side's original version, if one had to lose, is returned in
//     Backups, for the caller to write next to the file as
//     <name>.from-remote-<random> once the merge is committed. Backups are
//     git-ignored, so they stay on this PC for the user to review.
//   - A file one side changed and the other deleted: the changed copy is
//     kept, the same rule Reconcile uses (an edit outranks a delete). A file
//     that collided with a directory arrives this way too: git moves it aside
//     to <name>~<side>, and it is kept under that name.
//   - A path only the base has (deleted on both sides, e.g. a rename/rename):
//     removed.
//
// Paths are passed to git as literal pathspecs. On error nothing is
// guaranteed about the index; the caller aborts the merge.
func resolveConflicts(repo *gitwt.Repo) (Resolution, error) {
	var res Resolution
	entries, err := repo.UnmergedEntries()
	if err != nil {
		return res, err
	}
	for _, e := range entries {
		abs := filepath.Join(repo.Dir, filepath.FromSlash(e.Path))
		switch {
		case e.HasOurs && e.HasTheirs:
			var base []byte
			if e.HasBase {
				if base, err = repo.ShowStage(1, e.Path); err != nil {
					return res, err
				}
			} else {
				// Added on both sides (add/add): git has no base. The two adds
				// often descend from one version anyway — two PCs migrating the
				// same project into its fingerprint key, say. Use it as the base
				// so only real edits conflict, not the whole file.
				base = sharedVersion(repo, "MERGE_HEAD", e.Path, "HEAD", e.Path)
			}
			ours, err := repo.ShowStage(2, e.Path)
			if err != nil {
				return res, err
			}
			theirs, err := repo.ShowStage(3, e.Path)
			if err != nil {
				return res, err
			}

			var out, loser []byte
			var how string
			if path.Base(e.Path) == "MEMORY.md" {
				merged, failed := merge.Merge(string(base), string(ours), string(theirs))
				if failed {
					return res, fmt.Errorf("merge %s: git merge-file could not run", e.Path)
				}
				out, how = []byte(merged), "MEMORY.md merge"
			} else {
				theirsLater := changedLater(repo, "MERGE_HEAD", e.Path, "HEAD", e.Path)
				r := resolveText(base, ours, theirs, theirsLater)
				out, loser, how = r.content, r.loser, r.how
			}
			if err := writeFile(abs, out); err != nil {
				return res, err
			}
			if loser != nil {
				res.Backups = append(res.Backups, Backup{Path: e.Path + ".from-remote-" + randSuffix(), Content: loser})
			}
			if _, err := repo.Run("add", "--", literal(e.Path)); err != nil {
				return res, err
			}
			res.Files = append(res.Files, ResolvedFile{Path: e.Path, How: how})

		case e.HasOurs || e.HasTheirs:
			// Modified on one side, deleted on the other: keep the modified copy.
			stage, how := 2, "kept ours over delete"
			if e.HasTheirs {
				stage, how = 3, "kept theirs over delete"
			}
			content, err := repo.ShowStage(stage, e.Path)
			if err != nil {
				return res, err
			}
			if err := writeFile(abs, content); err != nil {
				return res, err
			}
			if _, err := repo.Run("add", "--", literal(e.Path)); err != nil {
				return res, err
			}
			res.Files = append(res.Files, ResolvedFile{Path: e.Path, How: how})

		case e.HasBase:
			// Deleted on both sides (e.g. a rename/rename): nothing to keep.
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return res, err
			}
			if _, err := repo.Run("rm", "--cached", "--quiet", "--", literal(e.Path)); err != nil {
				return res, err
			}
			res.Files = append(res.Files, ResolvedFile{Path: e.Path, How: "deleted on both sides"})

		default:
			return res, fmt.Errorf("unmerged %s has no stages", e.Path)
		}
	}
	return res, nil
}

// textResolution is resolveText's result: the content to stage, the losing
// side's original version (nil when nothing was lost), and a short label.
type textResolution struct {
	content []byte
	loser   []byte
	how     string
}

// resolveText settles a file both sides changed.
//
//  1. Each side's frontmatter `modified:` timestamp is read.
//  2. Every version's `modified:` line is set to the newest value, so the
//     timestamp alone is never a conflict.
//  3. A plain 3-way merge of those copies that comes out clean is used as is,
//     with no backup: only the timestamp differed, or the edits didn't
//     overlap.
//  4. Otherwise the side with the newer timestamp wins every conflicting hunk
//     (keeping the other side's non-overlapping changes). When the timestamps
//     can't decide — either side has none, or they are equal — theirsLater
//     does: the side whose last change in git history is more recent wins,
//     and this PC wins only if that too is a tie (or unknown). Without that
//     fallback, a stale local copy with no timestamp silently beat a newer
//     edit from another PC. The loser's original, unmodified version is
//     returned as the backup.
//
// Binary content (a NUL byte, as git judges it) can't be merged line by line,
// and neither can anything when `git merge-file` fails to run: then the
// winner, chosen the same way, is taken as a whole file. resolveText never
// fails, so no conflict of this kind can stop a merge from completing.
func resolveText(base, ours, theirs []byte, theirsLater bool) textResolution {
	rawO, tO, okO := modifiedOf(ours)
	rawT, tT, okT := modifiedOf(theirs)
	theirsNewer := theirsLater
	if okO && okT && !tT.Equal(tO) {
		theirsNewer = tT.After(tO)
	}

	if isBinary(base) || isBinary(ours) || isBinary(theirs) {
		return wholeFile(ours, theirs, theirsNewer, "binary")
	}

	newest := "" // the newest `modified:` value, from the timestamps alone
	switch {
	case okO && okT && tT.After(tO):
		newest = rawT
	case okO:
		newest = rawO
	case okT:
		newest = rawT
	}
	b2, o2, t2 := base, ours, theirs
	if newest != "" {
		b2, o2, t2 = setModified(base, newest), setModified(ours, newest), setModified(theirs, newest)
	}

	out, conflicts, err := merge.MergeFile(b2, o2, t2, merge.FavorNone)
	if err != nil {
		return wholeFile(ours, theirs, theirsNewer, "merge-file failed")
	}
	if conflicts == 0 {
		return textResolution{content: out, how: "merged"}
	}

	favor, loser, how := merge.FavorOurs, theirs, "kept ours"
	if theirsNewer {
		favor, loser, how = merge.FavorTheirs, ours, "kept theirs (newer)"
	}
	out, _, err = merge.MergeFile(b2, o2, t2, favor)
	if err != nil {
		return wholeFile(ours, theirs, theirsNewer, "merge-file failed")
	}
	return textResolution{content: out, loser: loser, how: how}
}

// wholeFile picks one side's complete version: theirs when it is newer,
// otherwise ours. The other side is the backup unless the two are identical.
func wholeFile(ours, theirs []byte, theirsNewer bool, why string) textResolution {
	win, lose, how := ours, theirs, "kept ours whole ("+why+")"
	if theirsNewer {
		win, lose, how = theirs, ours, "kept theirs whole ("+why+", newer)"
	}
	if bytes.Equal(win, lose) {
		lose = nil
	}
	return textResolution{content: win, loser: lose, how: how}
}

// isBinary applies git's own test: a NUL byte in the first 8000 bytes.
func isBinary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// modifiedOf returns the raw value of the frontmatter `modified:` key and its
// parsed time. ok is false when there is no frontmatter, no such key, or the
// value doesn't parse.
func modifiedOf(content []byte) (raw string, t time.Time, ok bool) {
	start, end, found := findModified(content)
	if !found {
		return "", time.Time{}, false
	}
	raw = string(content[start:end])
	t, ok = parseStamp(raw)
	return raw, t, ok
}

// setModified replaces the value of the frontmatter `modified:` key with raw.
// Content without that key is returned unchanged.
func setModified(content []byte, raw string) []byte {
	start, end, found := findModified(content)
	if !found {
		return content
	}
	out := make([]byte, 0, len(content)-(end-start)+len(raw))
	out = append(out, content[:start]...)
	out = append(out, raw...)
	out = append(out, content[end:]...)
	return out
}

// findModified locates the value of the `modified:` key in a leading YAML
// frontmatter block (between a first line of `---` and the next `---` line).
// The key counts at the top level or indented under a top-level `metadata:`
// key. It returns the byte range of the value, without surrounding blanks.
func findModified(content []byte) (start, end int, ok bool) {
	s := string(content)
	pos := 0
	if strings.HasPrefix(s, "\ufeff") {
		pos = len("\ufeff")
	}
	line, next := nextLine(s, pos)
	if strings.TrimRight(line, "\r") != "---" {
		return 0, 0, false
	}
	pos = next
	inMetadata := false
	found := false
	for pos < len(s) {
		line, next = nextLine(s, pos)
		l := strings.TrimRight(line, "\r")
		if l == "---" {
			return start, end, found
		}
		trimmed := strings.TrimLeft(l, " \t")
		indent := len(l) - len(trimmed)
		if indent == 0 && trimmed != "" {
			inMetadata = strings.HasPrefix(trimmed, "metadata:")
		}
		if !found && strings.HasPrefix(trimmed, "modified:") && (indent == 0 || inMetadata) {
			vs := pos + indent + len("modified:")
			ve := pos + len(l)
			for vs < ve && (s[vs] == ' ' || s[vs] == '\t') {
				vs++
			}
			ve = valueEnd(s, vs, ve)
			start, end, found = vs, ve, true
		}
		pos = next
	}
	return 0, 0, false // frontmatter never closed
}

// valueEnd returns where the YAML scalar starting at s[vs:ve] ends: after its
// closing quote when quoted, else before a ` #` comment, without trailing
// blanks. The comment is thus left out of the value and kept on rewrite.
func valueEnd(s string, vs, ve int) int {
	if vs < ve && (s[vs] == '"' || s[vs] == '\'') {
		if j := strings.IndexByte(s[vs+1:ve], s[vs]); j >= 0 {
			return vs + 1 + j + 1
		}
	} else {
		for i := vs; i < ve; i++ {
			if s[i] == '#' && i > vs && (s[i-1] == ' ' || s[i-1] == '\t') {
				ve = i
				break
			}
		}
	}
	for ve > vs && (s[ve-1] == ' ' || s[ve-1] == '\t') {
		ve--
	}
	return ve
}

// nextLine returns the line starting at pos (without its '\n') and the
// offset of the following line.
func nextLine(s string, pos int) (string, int) {
	if i := strings.IndexByte(s[pos:], '\n'); i >= 0 {
		return s[pos : pos+i], pos + i + 1
	}
	return s[pos:], len(s)
}

// parseStamp parses a frontmatter timestamp: RFC 3339, optionally quoted.
// A bare date or a date-time without a zone is accepted too, read as UTC.
func parseStamp(raw string) (time.Time, bool) {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
