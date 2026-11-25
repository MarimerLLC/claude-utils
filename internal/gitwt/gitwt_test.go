package gitwt

import (
	"reflect"
	"testing"
)

func TestParseUnmerged(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	rec := func(stage, path string) string { return "100644 " + sha + " " + stage + "\t" + path + "\x00" }
	out := rec("1", "projects/k/memory/a.md") + rec("2", "projects/k/memory/a.md") + rec("3", "projects/k/memory/a.md") +
		rec("2", "odd name\twith tab\nand newline.md") + // modify (ours) / delete (theirs)
		rec("1", "both-added-not.md") + rec("3", "both-added-not.md") + // delete (ours) / modify (theirs)
		rec("2", "added.md") + rec("3", "added.md") // add/add

	got, err := parseUnmerged(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Unmerged{
		{Path: "projects/k/memory/a.md", HasBase: true, HasOurs: true, HasTheirs: true},
		{Path: "odd name\twith tab\nand newline.md", HasOurs: true},
		{Path: "both-added-not.md", HasBase: true, HasTheirs: true},
		{Path: "added.md", HasOurs: true, HasTheirs: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseUnmerged_Empty(t *testing.T) {
	got, err := parseUnmerged("")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestParseUnmerged_Malformed(t *testing.T) {
	for _, in := range []string{"no tab here\x00", "100644 abc\tpath\x00", "100644 abc 9\tpath\x00"} {
		if _, err := parseUnmerged(in); err == nil {
			t.Errorf("parseUnmerged(%q) should fail", in)
		}
	}
}
