package sync

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFormatHealth(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name          string
		h             Health
		ahead, behind int
		want          string
	}{
		{
			name: "never synced",
			h:    Health{},
			want: "sync: no sync recorded yet (up to date with origin)",
		},
		{
			name: "ok, in step",
			h:    Health{LastAttempt: at("2026-11-24T17:55:00Z"), LastSuccess: at("2026-11-24T17:50:00Z")},
			want: "sync: ok — last synced 2026-11-24 17:50 (up to date with origin)",
		},
		{
			name:  "ok, ahead",
			h:     Health{LastAttempt: at("2026-11-24T17:50:00Z"), LastSuccess: at("2026-11-24T17:50:00Z")},
			ahead: 2,
			want:  "sync: ok — last synced 2026-11-24 17:50 (ahead 2, behind 0)",
		},
		{
			name: "failing",
			h: Health{
				LastAttempt:         at("2026-11-24T17:50:00Z"),
				LastSuccess:         at("2026-08-19T16:00:00Z"),
				ConsecutiveFailures: 412,
				FirstFailure:        at("2026-08-19T17:01:00Z"),
				LastError:           "pull: git pull:\n  CONFLICT (content)\tin x.md",
			},
			ahead: 231, behind: 360,
			want: "sync: FAILING — 412 consecutive failures since 2026-08-19 17:01 (ahead 231, behind 360)\n" +
				"  last success: 2026-08-19 16:00\n" +
				"  last error: pull: git pull: CONFLICT (content) in x.md",
		},
		{
			name: "one failure, never succeeded, position unknown",
			h: Health{
				LastAttempt:         at("2026-11-24T17:50:00Z"),
				ConsecutiveFailures: 1,
				FirstFailure:        at("2026-11-24T17:50:00Z"),
				LastError:           "ls-remote: boom",
			},
			ahead: -1, behind: -1,
			want: "sync: FAILING — 1 failure since 2026-11-24 17:50 (ahead/behind unknown)\n" +
				"  last error: ls-remote: boom",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FormatHealth(c.h, c.ahead, c.behind, time.UTC); got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

func TestFormatHealth_LongErrorTruncated(t *testing.T) {
	h := Health{LastAttempt: time.Now(), ConsecutiveFailures: 3, FirstFailure: time.Now(), LastError: strings.Repeat("x", 1000)}
	got := FormatHealth(h, 0, 0, time.UTC)
	if !strings.HasSuffix(got, "…") || len(got) > 600 {
		t.Fatalf("error not truncated: %d bytes", len(got))
	}
}

func TestHealthRecordAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)

	var h Health
	h.Record(t1, errors.New("one"))
	h.Record(t2, errors.New("two"))
	if err := h.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadHealth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsecutiveFailures != 2 || !got.FirstFailure.Equal(t1) || got.LastError != "two" || !got.LastAttempt.Equal(t2) {
		t.Fatalf("after two failures: %+v", got)
	}
	got.Record(t3, nil)
	if got.ConsecutiveFailures != 0 || !got.FirstFailure.IsZero() || got.LastError != "" || !got.LastSuccess.Equal(t3) {
		t.Fatalf("after success: %+v", got)
	}

	if h, err := LoadHealth(t.TempDir()); err != nil || !h.LastAttempt.IsZero() {
		t.Fatalf("missing file should give zero Health: %+v, %v", h, err)
	}
}
