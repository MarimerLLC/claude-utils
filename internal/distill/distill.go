// Package distill maintains the shared catalog of environment-level Claude Code
// memories.
//
// Individual memory files live per-project under
// ~/.claude/projects/<hash>/memory/. Some lessons are transferable across
// projects (shell/OS quirks, CLI gotchas, user identity) rather than bound to
// one repo. The /distill skill classifies and generalizes those, writing one
// catalog entry per lesson into the distilled directory (default
// ~/.claudesync/distilled/) and tagging the originating memory with a marker
// (default: scope: environment).
//
// This package is the mechanical half: it cannot classify (no model in the
// loop), it only indexes and reconciles what the skill produced. BuildIndex
// regenerates the human-readable DISTILLED.md index from the entry files;
// Reconcile prunes entries whose source memory lost the marker or vanished;
// analyzeSources surfaces a worklist of tagged-but-not-yet-distilled memories.
//
// The distilled directory sits inside the claude-memsync work-tree, so the
// daemon's existing `git add -A` carries the catalog across workstations with
// no extra transport.
package distill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IndexFileName is the generated catalog index written into the distilled dir.
const IndexFileName = "DISTILLED.md"

// Marker is the frontmatter key/value that gates a memory into the catalog.
type Marker struct {
	Key   string
	Value string
}

// DefaultMarker is the marker the /distill skill writes onto promoted memories.
var DefaultMarker = Marker{Key: "scope", Value: "environment"}

// Options configures a distill run.
type Options struct {
	// ProjectsDir is the root of Claude's per-project memory tree
	// (default: ~/.claude/projects). Used to analyze and reconcile sources.
	ProjectsDir string
	// DistilledDir holds the catalog entry files and the generated index
	// (default: ~/.claudesync/distilled).
	DistilledDir string
	// Marker gates which source memories belong in the catalog. Zero value
	// means DefaultMarker.
	Marker Marker
}

func (o *Options) applyDefaults() {
	if o.Marker.Key == "" {
		o.Marker = DefaultMarker
	}
}

// Entry is a parsed catalog entry (one distilled lesson).
type Entry struct {
	Name          string
	Description   string
	Type          string
	Scope         string
	OriginProject string
	OriginFile    string
	BodyHash      string // short hash of the trimmed body, for dedupe/conflict checks
	Path          string // absolute path to the entry file
}

// Origin identifies a source memory found while scanning the projects tree.
type Origin struct {
	Project string // <hash> directory name
	File    string // file name within memory/
	Path    string // absolute path
	Name    string // frontmatter name (falls back to the file stem)
}

// Conflict records a memory name carried by multiple sources with divergent
// content. The mechanical layer never merges prose; the /distill skill resolves
// these semantically.
type Conflict struct {
	Name    string
	Sources []string // "project/file" labels
}

// Result summarizes a distill run.
type Result struct {
	Indexed   int        // catalog entries written to the index
	Pruned    int        // stale catalog entries removed (Reconcile)
	Pending   []Origin   // marked source memories with no catalog entry yet
	Conflicts []Conflict // same name, divergent content across sources
}

// Run reconciles (when prune is set) and then rebuilds the index. This is the
// entry point used by `claude-memsync distill`.
func Run(opts Options, prune bool) (Result, error) {
	opts.applyDefaults()
	var pruned int
	if prune {
		r, err := Reconcile(opts)
		if err != nil {
			return Result{}, err
		}
		pruned = r.Pruned
	}
	res, err := BuildIndex(opts)
	if err != nil {
		return res, err
	}
	res.Pruned = pruned
	return res, nil
}

// Preview reports what BuildIndex would record (entry count, worklist,
// conflicts) without writing the index or touching any files. Used by
// `claude-memsync distill --dry-run`.
func Preview(opts Options) (Result, error) {
	opts.applyDefaults()
	entries, err := scanCatalog(opts.DistilledDir)
	if err != nil {
		return Result{}, fmt.Errorf("scan catalog: %w", err)
	}
	return analyze(opts, entries), nil
}

// BuildIndex parses every entry file in DistilledDir, regenerates the
// DISTILLED.md index, and reports the source worklist and any conflicts.
func BuildIndex(opts Options) (Result, error) {
	opts.applyDefaults()

	entries, err := scanCatalog(opts.DistilledDir)
	if err != nil {
		return Result{}, fmt.Errorf("scan catalog: %w", err)
	}
	if err := writeIndex(opts.DistilledDir, entries); err != nil {
		return Result{}, fmt.Errorf("write index: %w", err)
	}
	return analyze(opts, entries), nil
}

// analyze derives the Result (counts, conflicts, worklist) from already-scanned
// catalog entries. Read-only.
func analyze(opts Options, entries []Entry) Result {
	res := Result{Indexed: len(entries)}
	// Catalog-level conflict: two entry files declaring the same name.
	res.Conflicts = append(res.Conflicts, catalogConflicts(entries)...)
	// Source-level worklist + conflicts (best-effort; skipped if the projects
	// tree is unreadable, e.g. on a consume-only workstation).
	pending, conflicts := analyzeSources(opts, entries)
	res.Pending = pending
	res.Conflicts = append(res.Conflicts, conflicts...)
	return res
}

// Reconcile removes catalog entries whose originating memory no longer carries
// the marker or no longer exists. It is conservative: if the projects tree is
// not visible at all, it prunes nothing (avoids wiping the catalog on a machine
// that only consumes it).
func Reconcile(opts Options) (Result, error) {
	opts.applyDefaults()
	var res Result

	if _, err := os.Stat(opts.ProjectsDir); err != nil {
		return res, nil // sources not visible here; never prune blind
	}

	entries, err := scanCatalog(opts.DistilledDir)
	if err != nil {
		return res, fmt.Errorf("scan catalog: %w", err)
	}
	for _, e := range entries {
		if e.OriginProject == "" || e.OriginFile == "" {
			continue // hand-authored entry with no source; leave it alone
		}
		src := filepath.Join(opts.ProjectsDir, e.OriginProject, "memory", e.OriginFile)
		content, err := os.ReadFile(src)
		stale := false
		switch {
		case errors.Is(err, fs.ErrNotExist):
			stale = true
		case err != nil:
			continue // transient read error; don't prune on uncertainty
		default:
			meta, _, _ := parseFrontmatter(string(content))
			stale = meta[opts.Marker.Key] != opts.Marker.Value
		}
		if stale {
			if err := os.Remove(e.Path); err != nil {
				return res, fmt.Errorf("prune %s: %w", e.Path, err)
			}
			res.Pruned++
		}
	}
	return res, nil
}

// scanCatalog loads every entry file in dir, sorted by name. A missing dir is
// not an error (returns no entries).
func scanCatalog(dir string) ([]Entry, error) {
	files, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, f := range files {
		if !isEntryFile(f) {
			continue
		}
		e, err := loadEntry(filepath.Join(dir, f.Name()))
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func loadEntry(path string) (Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	meta, body, _ := parseFrontmatter(string(b))
	name := meta["name"]
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	return Entry{
		Name:          name,
		Description:   meta["description"],
		Type:          meta["type"],
		Scope:         meta["scope"],
		OriginProject: meta["originProject"],
		OriginFile:    meta["originFile"],
		BodyHash:      bodyHash(body),
		Path:          path,
	}, nil
}

// analyzeSources walks the projects tree for memories carrying the marker. It
// returns the ones with no catalog entry yet (pending) and any name carried by
// multiple sources with divergent bodies (conflicts). Unreadable trees yield
// empty results rather than errors.
func analyzeSources(opts Options, catalog []Entry) ([]Origin, []Conflict) {
	inCatalog := make(map[string]bool, len(catalog))
	for _, e := range catalog {
		inCatalog[e.Name] = true
	}

	dirs, err := os.ReadDir(opts.ProjectsDir)
	if err != nil {
		return nil, nil
	}

	type src struct {
		origin Origin
		hash   string
	}
	byName := map[string][]src{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		memDir := filepath.Join(opts.ProjectsDir, d.Name(), "memory")
		files, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !isEntryFile(f) {
				continue
			}
			path := filepath.Join(memDir, f.Name())
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			meta, body, _ := parseFrontmatter(string(content))
			if meta[opts.Marker.Key] != opts.Marker.Value {
				continue
			}
			name := meta["name"]
			if name == "" {
				name = strings.TrimSuffix(f.Name(), ".md")
			}
			byName[name] = append(byName[name], src{
				origin: Origin{Project: d.Name(), File: f.Name(), Path: path, Name: name},
				hash:   bodyHash(body),
			})
		}
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	var pending []Origin
	var conflicts []Conflict
	for _, n := range names {
		ss := byName[n]
		hashes := map[string]bool{}
		var labels []string
		for _, s := range ss {
			hashes[s.hash] = true
			labels = append(labels, s.origin.Project+"/"+s.origin.File)
		}
		if len(hashes) > 1 {
			conflicts = append(conflicts, Conflict{Name: n, Sources: labels})
		}
		if !inCatalog[n] {
			pending = append(pending, ss[0].origin)
		}
	}
	return pending, conflicts
}

func catalogConflicts(entries []Entry) []Conflict {
	byName := map[string][]Entry{}
	for _, e := range entries {
		byName[e.Name] = append(byName[e.Name], e)
	}
	var out []Conflict
	for name, es := range byName {
		if len(es) < 2 {
			continue
		}
		hashes := map[string]bool{}
		var labels []string
		for _, e := range es {
			hashes[e.BodyHash] = true
			labels = append(labels, filepath.Base(e.Path))
		}
		if len(hashes) > 1 {
			out = append(out, Conflict{Name: name, Sources: labels})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func writeIndex(dir string, entries []Entry) error {
	var b strings.Builder
	b.WriteString("# Distilled environment memories\n\n")
	b.WriteString("<!-- generated by `claude-memsync distill`; do not edit by hand -->\n\n")
	if len(entries) == 0 {
		b.WriteString("_No distilled memories yet._\n")
	} else {
		b.WriteString("| Name | Description | Type | Origin |\n")
		b.WriteString("|------|-------------|------|--------|\n")
		for _, e := range entries {
			b.WriteString(fmt.Sprintf("| [%s](%s) | %s | %s | %s |\n",
				e.Name,
				filepath.Base(e.Path),
				cellEscape(e.Description),
				dash(e.Type),
				dash(e.OriginProject),
			))
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, IndexFileName), []byte(b.String()), 0600)
}

// parseFrontmatter extracts a flat key/value map from leading YAML frontmatter
// and returns the remaining body. It is deliberately tolerant: it accepts both
// the flat `type: feedback` form (older memories) and keys nested one level
// under `metadata:` (current schema), flattening both into the same map. Inline
// objects and deeper nesting are ignored. Content without frontmatter is
// returned verbatim as the body with an empty map.
func parseFrontmatter(content string) (map[string]string, string, error) {
	meta := map[string]string{}
	if !strings.HasPrefix(content, "---") {
		return meta, content, nil
	}
	lines := strings.Split(content, "\n")
	if strings.TrimRight(lines[0], "\r") != "---" {
		return meta, content, nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return meta, content, nil // unterminated; treat as no frontmatter
	}
	for i := 1; i < end; i++ {
		line := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		if key == "metadata" && val == "" {
			continue // container line; its children are flattened in
		}
		val = strings.Trim(val, `"'`)
		if val != "" {
			meta[key] = val
		}
	}
	body := strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\n")
	return meta, body, nil
}

func isEntryFile(f fs.DirEntry) bool {
	if f.IsDir() {
		return false
	}
	n := f.Name()
	if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".md") {
		return false
	}
	if strings.Contains(n, ".tmp.") {
		return false // leftover merge-driver temp files
	}
	return n != "MEMORY.md" && n != IndexFileName
}

func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:8])
}

func cellEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
