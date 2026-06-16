package sync

import "testing"

func TestShouldIgnoreFile(t *testing.T) {
	ignore := []string{
		"",                                                // empty
		".hidden",                                         // dot-prefixed
		".gitignore",                                      // dot-prefixed
		"MEMORY.md.tmp",                                   // trailing .tmp
		"MEMORY.md.tmp.7368.beeb3e905e2a",                 // <name>.tmp.<pid>.<hash>
		"feedback_mingw_kubectl.md.tmp.27244.fac4861e524", // same, real-world shape
		"release_process.md.tmp.47284.4cad.from-remote-1", // temp + conflict-backup suffix
	}
	for _, n := range ignore {
		if !shouldIgnoreFile(n) {
			t.Errorf("shouldIgnoreFile(%q) = false, want true", n)
		}
	}

	keep := []string{
		"MEMORY.md",
		"feedback_mingw_kubectl.md",
		"reference-meai-cached-tokens.md",
		"project_routing_log_missing_tokens.md",
	}
	for _, n := range keep {
		if shouldIgnoreFile(n) {
			t.Errorf("shouldIgnoreFile(%q) = true, want false", n)
		}
	}
}
