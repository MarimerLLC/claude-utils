// Package merge implements semantic 3-way merge for Claude Code MEMORY.md files.
//
// MEMORY.md content typically takes one of two shapes:
//   - Strict-index: a flat list of "- [Title](file.md) — hook" entries (per
//     Claude Code's auto-memory system prompt).
//   - Free-form sectioned: H2-headed sections with arbitrary markdown bodies
//     (a common organic style for project knowledge).
//
// Both shapes split cleanly on H2 (^##\s) headings. We parse a file into
// blocks (a leading preamble plus zero or more sections), then merge by
// section key. Within a section, if both bodies are pure markdown lists,
// we union by the link target (or full line). Otherwise we shell out to
// `git merge-file --union` for a 3-way text merge of the body that keeps both
// sides' lines where they overlap, so the result never holds conflict markers.
package merge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// PreambleKey is the synthetic key used for content before the first H2 heading.
const PreambleKey = "__preamble__"

// Block represents one parsed section (or the preamble) of a MEMORY.md file.
type Block struct {
	Heading string // full heading line including trailing newline; "" for preamble
	Key     string // normalized key used to match blocks across versions
	Body    string // raw body content from after heading line up to next heading
}

// dupKeySep separates a heading key from its occurrence number when one
// document repeats the same H2 (or two headings that differ only in case,
// since keys are lowercased). Without this, blocksByKey would collapse them
// into a single map entry and every section but the last would be dropped —
// silent data loss, even on a merge where nothing actually changed.
//
// The separator is a NUL byte because it cannot occur in real markdown, so a
// disambiguated key can never collide with a genuine heading's key. Keys are
// internal to the merge algorithm and never appear in the output; only
// Block.Heading is written.
const dupKeySep = "\x00#"

var headingRe = regexp.MustCompile(`(?m)^##[ \t]+.*$`)

// Parse splits MEMORY.md content into blocks keyed by H2 heading. Repeated
// headings within one document are disambiguated by occurrence, so no block
// is lost when blocks are mapped by key. Cross-document matching stays
// case-insensitive; only same-document repeats are separated. Merge then
// re-keys repeats with alignRepeats, so a copy is matched to the copy it came
// from rather than to whichever copy sits at the same position.
func Parse(content string) []Block {
	indices := headingRe.FindAllStringIndex(content, -1)
	if len(indices) == 0 {
		return []Block{{Heading: "", Key: PreambleKey, Body: content}}
	}
	var blocks []Block
	if indices[0][0] > 0 {
		blocks = append(blocks, Block{Heading: "", Key: PreambleKey, Body: content[:indices[0][0]]})
	}
	occurrences := map[string]int{}
	for i, idx := range indices {
		start := idx[0]
		end := len(content)
		if i+1 < len(indices) {
			end = indices[i+1][0]
		}
		// Heading line is from start through the newline (inclusive if present).
		hdrEnd := strings.Index(content[start:end], "\n")
		var heading, body string
		if hdrEnd < 0 {
			heading = content[start:end]
		} else {
			heading = content[start : start+hdrEnd+1]
			body = content[start+hdrEnd+1 : end]
		}
		key := normalizeKey(strings.TrimSpace(strings.TrimLeft(strings.TrimRight(heading, "\r\n"), "#")))
		occurrences[key]++
		// First occurrence keeps the plain key so cross-document matching is
		// unaffected; repeats get an occurrence-numbered variant.
		if n := occurrences[key]; n > 1 {
			key = fmt.Sprintf("%s%s%d", key, dupKeySep, n)
		}
		blocks = append(blocks, Block{Heading: heading, Key: key, Body: body})
	}
	return blocks
}

func normalizeKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// bodyEqual compares two bodies ignoring trailing whitespace. This treats
// the blank-line separator that the parser attributes to whichever block
// precedes a new section as not-a-change.
func bodyEqual(a, b string) bool {
	return strings.TrimRight(a, " \t\r\n") == strings.TrimRight(b, " \t\r\n")
}

// Merge performs a 3-way semantic merge of MEMORY.md content.
// Returns the merged content and whether it is unusable: that happens only
// when git could not be run to merge a section body. Overlapping edits keep
// both versions instead of producing conflict markers.
func Merge(base, ours, theirs string) (string, bool) {
	bb := Parse(base)
	ob := Parse(ours)
	tb := Parse(theirs)
	alignRepeats(bb, ob, tb)

	baseMap := blocksByKey(bb)
	oursMap := blocksByKey(ob)
	theirsMap := blocksByKey(tb)

	// Output order: keys in ours order first, then any theirs-only keys.
	seen := map[string]bool{}
	var order []string
	for _, b := range ob {
		if !seen[b.Key] {
			order = append(order, b.Key)
			seen[b.Key] = true
		}
	}
	for _, b := range tb {
		if !seen[b.Key] {
			order = append(order, b.Key)
			seen[b.Key] = true
		}
	}

	var out strings.Builder
	hadConflict := false
	first := true
	for _, key := range order {
		bO, inOurs := oursMap[key]
		bT, inTheirs := theirsMap[key]
		bB, inBase := baseMap[key]

		var result Block
		var keep bool
		var conflict bool

		switch {
		case inOurs && inTheirs:
			if bO.Heading == bT.Heading && bodyEqual(bO.Body, bT.Body) {
				result = bO
				keep = true
				break
			}
			// If one side is unchanged from base (modulo trailing whitespace),
			// the other side wins outright — no merge needed.
			if inBase && bO.Heading == bB.Heading && bodyEqual(bO.Body, bB.Body) {
				result = bT
				keep = true
				break
			}
			if inBase && bT.Heading == bB.Heading && bodyEqual(bT.Body, bB.Body) {
				result = bO
				keep = true
				break
			}
			heading := chooseHeading(bO, bT, bB, inBase)
			baseBody := ""
			if inBase {
				baseBody = bB.Body
			}
			merged, conf := mergeBodies(baseBody, bO.Body, bT.Body)
			result = Block{Heading: heading, Key: key, Body: merged}
			conflict = conf
			keep = true

		case inOurs && !inTheirs:
			// Theirs lacks the block. If base had it ~identical to ours, theirs deleted it — accept.
			if inBase && bO.Heading == bB.Heading && bodyEqual(bO.Body, bB.Body) {
				keep = false
			} else {
				result = bO
				keep = true
			}

		case !inOurs && inTheirs:
			if inBase && bT.Heading == bB.Heading && bodyEqual(bT.Body, bB.Body) {
				keep = false
			} else {
				result = bT
				keep = true
			}
		}

		if !keep {
			continue
		}
		body := strings.TrimRight(result.Body, "\r\n")
		if !first {
			out.WriteString("\n") // blank-line separator before each non-first block
		}
		out.WriteString(result.Heading)
		if body != "" {
			out.WriteString(body)
			out.WriteString("\n")
		}
		first = false
		if conflict {
			hadConflict = true
		}
	}
	return out.String(), hadConflict
}

func chooseHeading(o, t, b Block, inBase bool) string {
	if !inBase {
		return o.Heading
	}
	// If ours unchanged from base, prefer theirs heading; if theirs unchanged, prefer ours.
	if o.Heading == b.Heading {
		return t.Heading
	}
	if t.Heading == b.Heading {
		return o.Heading
	}
	return o.Heading
}

// groupKey returns the heading key a block was parsed under, without the
// occurrence suffix Parse adds to a repeated heading.
func groupKey(key string) string {
	g, _, _ := strings.Cut(key, dupKeySep)
	return g
}

// alignRepeats re-keys the copies of each repeated heading so that every copy
// in ours and theirs is matched to the base copy it came from, not merely to
// the copy at the same position. Parse numbers repeats by position alone,
// which goes wrong as soon as one side adds or deletes an earlier copy: every
// later copy shifts onto the wrong counterpart, and the merge then brings back
// deleted text or reports a conflict between unrelated sections.
//
// Copies are aligned the way a line diff aligns lines. Copies whose body is
// unchanged from a base copy are anchors, found as a longest common
// subsequence so their order is respected. Between two anchors, the remaining
// copies pair with the remaining base copies in order. Copies that neither
// side matched to base are aligned between ours and theirs the same way, and
// any copy still unmatched gets a key of its own, so it is kept.
//
// Headings that appear at most once in every version are left alone, so their
// matching is exactly what it was before.
func alignRepeats(base, ours, theirs []Block) {
	type copies struct{ base, ours, theirs []int }
	groups := map[string]*copies{}
	collect := func(blocks []Block, pick func(*copies) *[]int) {
		for i, b := range blocks {
			if b.Heading == "" {
				continue // preamble
			}
			g := groupKey(b.Key)
			if groups[g] == nil {
				groups[g] = &copies{}
			}
			p := pick(groups[g])
			*p = append(*p, i)
		}
	}
	collect(base, func(c *copies) *[]int { return &c.base })
	collect(ours, func(c *copies) *[]int { return &c.ours })
	collect(theirs, func(c *copies) *[]int { return &c.theirs })

	bodies := func(blocks []Block, idx []int) []string {
		out := make([]string, len(idx))
		for i, j := range idx {
			out[i] = blocks[j].Body
		}
		return out
	}

	for g, c := range groups {
		if len(c.base) <= 1 && len(c.ours) <= 1 && len(c.theirs) <= 1 {
			continue
		}
		baseBodies := bodies(base, c.base)

		// Match each side against base; base copies keep their Parse keys.
		leftovers := func(blocks []Block, idx []int) []int {
			var rest []int
			for i, m := range alignCopies(bodies(blocks, idx), baseBodies) {
				if m >= 0 {
					blocks[idx[i]].Key = base[c.base[m]].Key
				} else {
					rest = append(rest, idx[i])
				}
			}
			return rest
		}
		restO := leftovers(ours, c.ours)
		restT := leftovers(theirs, c.theirs)

		// Copies with no base counterpart: pair ours with theirs, so the same
		// copy added (or kept, with an empty base) on both sides is merged
		// rather than duplicated.
		pairedT := make([]bool, len(restT))
		for i, m := range alignCopies(bodies(ours, restO), bodies(theirs, restT)) {
			if m >= 0 {
				k := fmt.Sprintf("%s%snew%d", g, dupKeySep, i)
				ours[restO[i]].Key = k
				theirs[restT[m]].Key = k
				pairedT[m] = true
			} else {
				ours[restO[i]].Key = fmt.Sprintf("%s%sours%d", g, dupKeySep, i)
			}
		}
		for j, paired := range pairedT {
			if !paired {
				theirs[restT[j]].Key = fmt.Sprintf("%s%stheirs%d", g, dupKeySep, j)
			}
		}
	}
}

// alignCopies matches the bodies in a to the bodies in b, preserving order,
// and returns for each element of a the index of its match in b, or -1.
// Equal bodies are matched first (a longest common subsequence); between two
// such anchors, unmatched elements of a and b pair up in order.
func alignCopies(a, b []string) []int {
	n, m := len(a), len(b)
	// lcs[i][j] is the length of the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if bodyEqual(a[i], b[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	match := make([]int, n)
	for i := range match {
		match[i] = -1
	}
	var gapA, gapB []int
	flush := func() {
		for k := 0; k < len(gapA) && k < len(gapB); k++ {
			match[gapA[k]] = gapB[k]
		}
		gapA, gapB = gapA[:0], gapB[:0]
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case bodyEqual(a[i], b[j]):
			flush()
			match[i] = j
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			gapA = append(gapA, i)
			i++
		default:
			gapB = append(gapB, j)
			j++
		}
	}
	for ; i < n; i++ {
		gapA = append(gapA, i)
	}
	for ; j < m; j++ {
		gapB = append(gapB, j)
	}
	flush()
	return match
}

func blocksByKey(blocks []Block) map[string]Block {
	m := make(map[string]Block, len(blocks))
	for _, b := range blocks {
		m[b.Key] = b
	}
	return m
}

// mergeBodies merges three textual bodies. If both bodies are pure markdown
// lists, do a union merge by item key (link target or full line). Otherwise
// fall back to `git merge-file`.
func mergeBodies(base, ours, theirs string) (string, bool) {
	if isListy(ours) && isListy(theirs) {
		return mergeList(ours, theirs), false
	}
	return mergeDiff3(base, ours, theirs)
}

func isListy(body string) bool {
	lines := strings.Split(body, "\n")
	nonEmpty := 0
	listLines := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		nonEmpty++
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
			listLines++
		}
	}
	return nonEmpty > 0 && listLines == nonEmpty
}

var listLinkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

func listKey(line string) string {
	if m := listLinkRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return strings.TrimSpace(line)
}

// mergeList unions two list bodies, preserving the order from ours and
// appending any items only in theirs.
func mergeList(ours, theirs string) string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			k := listKey(l)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, l)
		}
	}
	add(ours)
	add(theirs)
	body := strings.Join(out, "\n")
	if strings.HasSuffix(ours, "\n") || strings.HasSuffix(theirs, "\n") {
		body += "\n"
	}
	return body
}

// mergeDiff3 runs a 3-way text merge of one section body with
// `git merge-file --union`: where both sides changed the same lines, both
// versions are kept (ours first), so MEMORY.md never gets conflict markers.
// It reports a conflict only when git itself could not be run; the result is
// then ours plus a comment saying so.
func mergeDiff3(base, ours, theirs string) (string, bool) {
	result, _, err := MergeFile([]byte(base), []byte(ours), []byte(theirs), FavorUnion)
	if err != nil {
		return ours + fmt.Sprintf("\n<!-- claude-memmerge: git merge-file failed: %s -->\n", err), true
	}
	return string(result), false
}

// Favor selects how MergeFile settles lines both sides changed.
type Favor string

const (
	FavorNone   Favor = ""       // leave conflict markers
	FavorOurs   Favor = "ours"   // take ours for each conflicting hunk
	FavorTheirs Favor = "theirs" // take theirs for each conflicting hunk
	FavorUnion  Favor = "union"  // keep both sides' lines for each conflicting hunk
)

// MergeFile runs `git merge-file -p` on the three versions and returns the
// merged content and the number of conflicting hunks (always 0 unless favor
// is FavorNone). err is non-nil only when git could not be run.
func MergeFile(base, ours, theirs []byte, favor Favor) ([]byte, int, error) {
	dir, err := os.MkdirTemp("", "memmerge-*")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(dir)

	bp := filepath.Join(dir, "base")
	op := filepath.Join(dir, "ours")
	tp := filepath.Join(dir, "theirs")
	for _, w := range []struct {
		path    string
		content []byte
	}{{bp, base}, {op, ours}, {tp, theirs}} {
		if err := os.WriteFile(w.path, w.content, 0600); err != nil {
			return nil, 0, err
		}
	}

	args := []string{"merge-file", "-p"}
	if favor != FavorNone {
		args = append(args, "--"+string(favor))
	}
	args = append(args, op, bp, tp)
	cmd := exec.Command("git", args...)
	proc.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		return stdout.Bytes(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 && exitErr.ExitCode() < 128 {
		// Positive exit code = number of conflicts (capped at 127).
		return stdout.Bytes(), exitErr.ExitCode(), nil
	}
	return nil, 0, fmt.Errorf("git merge-file: %w: %s", err, strings.TrimSpace(stderr.String()))
}
