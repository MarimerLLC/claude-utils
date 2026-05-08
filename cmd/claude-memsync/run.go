package main

import (
	"flag"
)

// runRun implements `claude-memsync run`. It delegates to the kardianos
// service runner, which works identically when launched interactively or
// by a system service supervisor (SCM / systemd / launchd).
func runRun(args []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	return runService(*cfgPath)
}
