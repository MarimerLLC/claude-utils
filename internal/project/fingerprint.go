package project

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Method records how a project's key was derived, for diagnostics in the index.
type Method string

const (
	MethodRemote Method = "remote" // normalized origin remote URL (preferred)
	MethodRoot   Method = "root"   // git root-commit SHA (git repo, no remote)
	MethodPath   Method = "path"   // fallback: Claude's dash-encoded dir name
)

// Key prefixes distinguish fingerprint keys from legacy path keys (which start
// with "-"), so migration can tell what to move and the two never collide.
const (
	remotePrefix = "g-"
	rootPrefix   = "r-"
)

// Fingerprint computes the machine-independent mirror key for the project at
// realPath. localHash is Claude's directory name, used verbatim as the path
// fallback so non-git projects keep today's per-machine behavior (they simply
// won't merge across machines, but never merge with an unrelated project).
//
// Preference order: normalized origin remote URL, then git root-commit SHA when
// there is no remote, then the path fallback.
//
// When realPath is a subdirectory of the work-tree rather than its top level,
// the path relative to the top level is folded into the key. Claude keeps a
// separate project (and memory) per directory, so a session in repo/docs must
// not share memories with one at the repo root — and a plain folder that merely
// sits inside some enclosing repo (a dotfiles-managed home, say) must not take
// on that repo's identity.
func Fingerprint(realPath, localHash string) (key string, method Method) {
	if realPath != "" {
		repo := gitwt.New(realPath)
		if repo.IsRepo() {
			sub := repoPrefix(repo)
			if url, ok := originURL(repo); ok {
				return remotePrefix + shortHash(NormalizeRemoteURL(url)+sub), MethodRemote
			}
			if root, ok := rootCommit(repo); ok {
				return rootPrefix + shortHash(root+sub), MethodRoot
			}
		}
	}
	return localHash, MethodPath
}

// repoPrefix returns "" when the repo's current directory is the work-tree top
// level, else "#<relative/path>" (lowercased, forward slashes, no trailing
// slash) to distinguish subdirectory projects. Lowercasing keeps the key stable
// across case-insensitive filesystems where the recorded cwd's casing can vary.
func repoPrefix(repo *gitwt.Repo) string {
	out, err := repo.Run("rev-parse", "--show-prefix")
	if err != nil {
		return ""
	}
	p := strings.Trim(strings.ReplaceAll(strings.TrimSpace(out), `\`, "/"), "/")
	if p == "" {
		return ""
	}
	return "#" + strings.ToLower(p)
}

func originURL(repo *gitwt.Repo) (string, bool) {
	out, err := repo.Run("config", "--get", "remote.origin.url")
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(out)
	return url, url != ""
}

// rootCommit returns a stable identifier built from the repository's root
// commit(s). A shallow clone's history is truncated, so its "root" is a
// per-clone boundary commit rather than the true first commit — useless as an
// identity — so we bail and let the caller fall back to the path key. Repos with
// multiple root commits (merged orphan histories) are handled by sorting and
// joining so the result is order-independent.
func rootCommit(repo *gitwt.Repo) (string, bool) {
	if out, err := repo.Run("rev-parse", "--is-shallow-repository"); err == nil {
		if strings.TrimSpace(out) == "true" {
			return "", false
		}
	}
	out, err := repo.Run("rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return "", false
	}
	roots := strings.Fields(out)
	if len(roots) == 0 {
		return "", false
	}
	sort.Strings(roots)
	return strings.Join(roots, ","), true
}

// NormalizeRemoteURL reduces the many equivalent spellings of a git remote to a
// single canonical "host/path" form so the same repository yields one key across
// machines and protocols. All of git@github.com:Acme/app.git,
// https://github.com/acme/app, and ssh://git@github.com:22/acme/app.git collapse
// to github.com/acme/app.
func NormalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)

	if i := strings.Index(s, "://"); i >= 0 {
		// scheme://[user@]host[:port]/path
		s = s[i+3:]
		if at := strings.Index(s, "@"); at >= 0 {
			if slash := strings.Index(s, "/"); slash < 0 || at < slash {
				s = s[at+1:]
			}
		}
	} else if colon := strings.Index(s, ":"); colon >= 0 {
		// scp-like: [user@]host:path
		host := s[:colon]
		path := s[colon+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		s = host + "/" + path
	}

	// Strip a :port from the host segment (everything before the first slash).
	if slash := strings.Index(s, "/"); slash > 0 {
		host, rest := s[:slash], s[slash:]
		if c := strings.Index(host, ":"); c >= 0 {
			host = host[:c]
		}
		s = host + rest
	}

	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git"), "/")
	return strings.ToLower(s)
}

// IsFingerprintKey reports whether name is a fingerprint key (g-/r- prefix
// followed by exactly shortHashLen lowercase hex digits) rather than a legacy
// path key. The exact shape matters: a Windows project on a lowercase drive
// letter encodes as e.g. "g--src-app", which must stay a path key. A fingerprint
// key can't be reversed to a local path, so a mirror dir with one and no
// matching local project must wait to materialize until that project is opened
// here.
func IsFingerprintKey(name string) bool {
	var rest string
	switch {
	case strings.HasPrefix(name, remotePrefix):
		rest = name[len(remotePrefix):]
	case strings.HasPrefix(name, rootPrefix):
		rest = name[len(rootPrefix):]
	default:
		return false
	}
	if len(rest) != shortHashLen {
		return false
	}
	for _, c := range rest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

const shortHashLen = 12

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:shortHashLen]
}
