//go:build !windows

// Command claude-focus is Windows-only; see main_windows.go.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "claude-focus: only supported on Windows")
	os.Exit(1)
}
