package project

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// decodeBudget caps how many directories DecodePath may list while searching,
// so a pathological name can't make an index build crawl the disk.
const decodeBudget = 4096

// EncodePath reproduces Claude's per-project directory name for an absolute
// path: every character other than an ASCII letter or digit becomes a dash
// (S:\src\rdl\csla -> S--src-rdl-csla, /Users/me/code/foo -> -Users-me-code-foo).
func EncodePath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// DecodePath recovers the directory a Claude project folder name stands for by
// searching the disk for an existing directory that encodes to it. The encoding
// is lossy (a dash may be a separator or part of a name, and '.', '_' and spaces
// also become dashes), so it succeeds only when exactly one existing directory
// matches; zero or several matches return ok=false, and callers keep the path
// key rather than guess.
//
// It is the fallback for project folders whose transcripts can't name the
// project's directory: folders that exist only because sync materialized them
// (never opened in Claude on this machine), and folders whose recorded cwd no
// longer exists.
func DecodePath(localHash string) (string, bool) {
	root, rest, ok := splitRoot(localHash)
	if !ok {
		return "", false
	}
	d := decoder{fold: runtime.GOOS != "linux", budget: decodeBudget}
	d.walk(root, rest)
	if len(d.found) != 1 || d.budget < 0 {
		return "", false
	}
	return d.found[0], true
}

// splitRoot separates the filesystem root from the rest of an encoded name:
// "S--src-rdl" -> ("S:\", "src-rdl") on Windows, "-home-me" -> ("/", "home-me")
// elsewhere.
func splitRoot(localHash string) (root, rest string, ok bool) {
	if runtime.GOOS == "windows" {
		if len(localHash) < 3 || localHash[1] != '-' || localHash[2] != '-' {
			return "", "", false
		}
		c := localHash[0]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return "", "", false
		}
		return string(c) + `:\`, localHash[3:], true
	}
	if !strings.HasPrefix(localHash, "-") {
		return "", "", false
	}
	return "/", localHash[1:], true
}

type decoder struct {
	fold   bool // case-insensitive file systems (Windows, macOS)
	budget int
	found  []string
}

// walk consumes rest one directory name at a time. A child matches when its
// encoded name equals rest, or is a prefix of rest followed by the dash that
// encodes the separator.
func (d *decoder) walk(dir, rest string) {
	if len(d.found) > 1 || d.budget < 0 {
		return // already ambiguous, or out of budget
	}
	if rest == "" {
		d.found = append(d.found, dir)
		return
	}
	d.budget--
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		enc := EncodePath(e.Name())
		switch {
		case d.equal(rest, enc):
			d.walk(filepath.Join(dir, e.Name()), "")
		case len(rest) > len(enc) && d.equal(rest[:len(enc)], enc) && rest[len(enc)] == '-':
			d.walk(filepath.Join(dir, e.Name()), rest[len(enc)+1:])
		}
	}
}

func (d *decoder) equal(a, b string) bool {
	if d.fold {
		return strings.EqualFold(a, b)
	}
	return a == b
}
