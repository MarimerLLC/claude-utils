package main

import (
	"fmt"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/merge"
	"github.com/MarimerLLC/claude-utils/internal/version"
)

// claude-memmerge is a git custom merge driver for MEMORY.md files.
//
// Git invokes it as: claude-memmerge %O %A %B [%L [%P]]
//   %O = ancestor (base) version
//   %A = current branch / "ours" — must be overwritten with merged result
//   %B = "theirs"
//   %L = conflict marker length (optional)
//   %P = pathname of file in working tree (optional)
//
// Exit 0 = clean merge (file at %A path now contains merged result).
// Exit non-zero = conflict; file at %A path may contain conflict markers.
//
// See: https://git-scm.com/docs/gitattributes#_defining_a_custom_merge_driver

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version.Strings("claude-memmerge"))
			return
		case "help", "--help", "-h":
			usage()
			return
		}
	}

	if len(os.Args) < 4 {
		usage()
		os.Exit(2)
	}

	basePath := os.Args[1]
	oursPath := os.Args[2]
	theirsPath := os.Args[3]

	base, err := os.ReadFile(basePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read base:", err)
		os.Exit(2)
	}
	ours, err := os.ReadFile(oursPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read ours:", err)
		os.Exit(2)
	}
	theirs, err := os.ReadFile(theirsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read theirs:", err)
		os.Exit(2)
	}

	merged, conflict := merge.Merge(string(base), string(ours), string(theirs))

	if err := os.WriteFile(oursPath, []byte(merged), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write ours:", err)
		os.Exit(2)
	}

	if conflict {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `claude-memmerge — git custom merge driver for MEMORY.md

Usage:
  claude-memmerge <base> <ours> <theirs> [marker-len] [pathname]

This binary is invoked by git via merge.<name>.driver configuration; not
typically run by hand.`)
}
