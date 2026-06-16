package main

import (
	"fmt"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version.Strings("claude-memsync"))
	case "help", "--help", "-h":
		usage()
	case "init":
		os.Exit(runInit(os.Args[2:]))
	case "run":
		os.Exit(runRun(os.Args[2:]))
	case "distill":
		os.Exit(runDistill(os.Args[2:]))
	case "install":
		os.Exit(runInstall(os.Args[2:]))
	case "uninstall":
		os.Exit(runUninstall(os.Args[2:]))
	case "start":
		os.Exit(runStart(os.Args[2:]))
	case "stop":
		os.Exit(runStop(os.Args[2:]))
	case "status":
		os.Exit(runStatus(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `claude-memsync — sync Claude Code memories across workstations

Usage:
  claude-memsync <subcommand> [args]

Subcommands:
  init        Bootstrap the local sync repo against a remote
  run         Run the sync daemon in the foreground
  distill     Rebuild the distilled-memory catalog index (see --prune, --dry-run)
  install     Install as a system service (Windows Service / systemd unit / launchd plist)
  uninstall   Remove the system service
  start       Start the system service
  stop        Stop the system service
  status      Report service status
  version     Print version
  help        Show this help`)
}
