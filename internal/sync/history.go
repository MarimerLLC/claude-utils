package sync

import (
	"strconv"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// zeroBlob reports whether id is git's all-zeros "no blob" object id, which
// --raw output uses for the missing side of an add or a delete.
func zeroBlob(id string) bool { return strings.Trim(id, "0") == "" }

// pathBlobs returns the distinct blob ids path has held in rev's history,
// newest first. Merge commits count too (-m), so a version that only ever
// existed as a merge result is found. On error it returns nil.
func pathBlobs(repo *gitwt.Repo, rev, path string) []string {
	out, err := repo.Run("log", "-m", "--format=", "--raw", "--no-abbrev", rev, "--", path)
	if err != nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		// ":100644 100644 <old-blob> <new-blob> M\tpath"
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[0], ":") {
			continue
		}
		if id := f[3]; !zeroBlob(id) && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// sharedVersion returns the newest version of pathA in revA's history that
// pathB has also held in revB's history, or nil when they share none.
//
// It stands in for the merge base git can't supply when two PCs created the
// same file independently (both migrating a project into its fingerprint key,
// say) even though the copies descend from one earlier version: given that
// version as the base, a 3-way merge sees which side actually changed.
func sharedVersion(repo *gitwt.Repo, revA, pathA, revB, pathB string) []byte {
	b := pathBlobs(repo, revB, pathB)
	if len(b) == 0 {
		return nil
	}
	inB := make(map[string]bool, len(b))
	for _, id := range b {
		inB[id] = true
	}
	for _, id := range pathBlobs(repo, revA, pathA) {
		if inB[id] {
			out, err := repo.Run("cat-file", "blob", id)
			if err != nil {
				return nil
			}
			return []byte(out)
		}
	}
	return nil
}

// lastChange returns the author time of the newest commit in rev's history
// that changed path. Author time, not commit time: a merge or rebase that
// carries a change over keeps when it was made.
func lastChange(repo *gitwt.Repo, rev, path string) (time.Time, bool) {
	out, err := repo.Run("log", "-1", "--format=%at", rev, "--", path)
	if err != nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// changedLater reports whether pathA (in revA) was last changed strictly after
// pathB (in revB). It is false when either time is unknown.
func changedLater(repo *gitwt.Repo, revA, pathA, revB, pathB string) bool {
	tA, okA := lastChange(repo, revA, pathA)
	tB, okB := lastChange(repo, revB, pathB)
	return okA && okB && tA.After(tB)
}
