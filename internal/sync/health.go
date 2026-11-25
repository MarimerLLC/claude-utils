package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Health is the daemon's record of how its syncs with origin are going, so
// `claude-memsync status` can tell a working daemon from one that is running
// but failing every sync. It lives at ~/.claudesync/.state/health.json
// (git-ignored, like the manifest).
type Health struct {
	LastAttempt         time.Time `json:"lastAttempt"`
	LastSuccess         time.Time `json:"lastSuccess"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	FirstFailure        time.Time `json:"firstFailure"` // first of the current run of failures
	LastError           string    `json:"lastError,omitempty"`
}

const healthFileName = "health.json"

// HealthPath returns the health file's path inside syncDir.
func HealthPath(syncDir string) string {
	return filepath.Join(syncDir, manifestStateDir, healthFileName)
}

// LoadHealth reads the health record; a missing file yields a zero Health.
func LoadHealth(syncDir string) (Health, error) {
	var h Health
	b, err := os.ReadFile(HealthPath(syncDir))
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return Health{}, err
	}
	return h, nil
}

// Save writes the health record to syncDir.
func (h Health) Save(syncDir string) error {
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(HealthPath(syncDir)), 0700); err != nil {
		return err
	}
	return writeAtomic(HealthPath(syncDir), b)
}

// Record notes the outcome of one sync attempt at now. A success clears the
// failure run; a failure starts one (keeping FirstFailure from its start).
func (h *Health) Record(now time.Time, err error) {
	h.LastAttempt = now
	if err == nil {
		h.LastSuccess = now
		h.ConsecutiveFailures = 0
		h.FirstFailure = time.Time{}
		h.LastError = ""
		return
	}
	if h.ConsecutiveFailures == 0 {
		h.FirstFailure = now
	}
	h.ConsecutiveFailures++
	h.LastError = err.Error()
}

// FormatHealth renders the sync line(s) of `claude-memsync status`. ahead and
// behind count commits against the local copy of origin's branch; pass -1 for
// either when unknown. Times are shown in loc.
func FormatHealth(h Health, ahead, behind int, loc *time.Location) string {
	stamp := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") }

	position := "ahead/behind unknown"
	switch {
	case ahead < 0 || behind < 0:
	case ahead == 0 && behind == 0:
		position = "up to date with origin"
	default:
		position = fmt.Sprintf("ahead %d, behind %d", ahead, behind)
	}

	if h.LastAttempt.IsZero() {
		return fmt.Sprintf("sync: no sync recorded yet (%s)", position)
	}
	if h.ConsecutiveFailures == 0 {
		return fmt.Sprintf("sync: ok — last synced %s (%s)", stamp(h.LastSuccess), position)
	}

	failures := "consecutive failures"
	if h.ConsecutiveFailures == 1 {
		failures = "failure"
	}
	out := fmt.Sprintf("sync: FAILING — %d %s since %s (%s)", h.ConsecutiveFailures, failures, stamp(h.FirstFailure), position)
	if !h.LastSuccess.IsZero() {
		out += fmt.Sprintf("\n  last success: %s", stamp(h.LastSuccess))
	}
	if h.LastError != "" {
		out += "\n  last error: " + oneLine(h.LastError, 400)
	}
	return out
}

// oneLine collapses whitespace runs (including newlines from git's stderr)
// to single spaces and truncates to max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
