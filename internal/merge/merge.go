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
// `git merge-file` for a standard 3-way text merge of the body.
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
)

// PreambleKey is the synthetic key used for content before the first H2 heading.
const PreambleKey = "__preamble__"

// Block represents one parsed section (or the preamble) of a MEMORY.md file.
type Block struct {
	Heading string // full heading line including trailing newline; "" for preamble
	Key     string // normalized key used to match blocks across versions
	Body    string // raw body content from after heading line up to next heading
}

var headingRe = regexp.MustCompile(`(?m)^##[ \t]+.*$`)

// Parse splits MEMORY.md content into blocks keyed by H2 heading.
func Parse(content string) []Block {
	indices := headingRe.FindAllStringIndex(content, -1)
	if len(indices) == 0 {
		return []Block{{Heading: "", Key: PreambleKey, Body: content}}
	}
	var blocks []Block
	if indices[0][0] > 0 {
		blocks = append(blocks, Block{Heading: "", Key: PreambleKey, Body: content[:indices[0][0]]})
	}
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
// Returns the merged content and whether unresolved conflicts remain.
func Merge(base, ours, theirs string) (string, bool) {
	bb := Parse(base)
	ob := Parse(ours)
	tb := Parse(theirs)

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

// mergeDiff3 shells out to `git merge-file -p` for a 3-way text merge.
// Returns the merged content (which may contain conflict markers) and whether
// any conflicts occurred.
func mergeDiff3(base, ours, theirs string) (string, bool) {
	dir, err := os.MkdirTemp("", "memmerge-*")
	if err != nil {
		return ours, true
	}
	defer os.RemoveAll(dir)

	bp := filepath.Join(dir, "base")
	op := filepath.Join(dir, "ours")
	tp := filepath.Join(dir, "theirs")
	for _, w := range []struct {
		path, content string
	}{{bp, base}, {op, ours}, {tp, theirs}} {
		if err := os.WriteFile(w.path, []byte(w.content), 0600); err != nil {
			return ours, true
		}
	}

	cmd := exec.Command("git", "merge-file", "-p", op, bp, tp)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	result := stdout.String()

	if err == nil {
		return result, false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Positive exit code = number of conflicts; negative = error.
		if exitErr.ExitCode() > 0 {
			return result, true
		}
	}
	// Hard error invoking git — fall back to ours with conflict marker block.
	return ours + fmt.Sprintf("\n<!-- claude-memmerge: git merge-file failed: %s -->\n", strings.TrimSpace(stderr.String())), true
}
