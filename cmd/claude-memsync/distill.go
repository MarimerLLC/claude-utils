package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/distill"
)

// runDistill implements `claude-memsync distill`. It regenerates the
// DISTILLED.md index from the catalog entry files the /distill skill produced,
// optionally prunes stale entries, and reports the worklist of marked-but-not-
// yet-distilled memories. It performs no classification — that is the skill's
// job; this is the mechanical half.
func runDistill(args []string) int {
	flags := flag.NewFlagSet("distill", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	prune := flags.Bool("prune", false, "remove catalog entries whose source memory lost the marker or vanished")
	dryRun := flags.Bool("dry-run", false, "report what would change without writing the index or pruning")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadDistillConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "distill:", err)
		return 1
	}
	opts := distill.Options{
		ProjectsDir:  cfg.ClaudeProjectsDir,
		DistilledDir: cfg.DistilledPath(),
	}

	var res distill.Result
	if *dryRun {
		res, err = distill.Preview(opts)
	} else {
		res, err = distill.Run(opts, *prune)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "distill:", err)
		return 1
	}

	report(res, *dryRun)
	return 0
}

// loadDistillConfig loads config.json, falling back to platform defaults when
// no config exists yet (the catalog can be indexed before `init` has run).
func loadDistillConfig(cfgPath string) (config.Config, error) {
	if cfgPath == "" {
		cfgPath = defaultConfigPath()
	}
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		return config.Defaults(), nil
	}
	return cfg, err
}

func report(res distill.Result, dryRun bool) {
	verb := "indexed"
	if dryRun {
		verb = "would index"
	}
	fmt.Printf("%s %d distilled %s\n", verb, res.Indexed, plural(res.Indexed, "entry", "entries"))
	if res.Pruned > 0 {
		fmt.Printf("pruned %d stale %s\n", res.Pruned, plural(res.Pruned, "entry", "entries"))
	}
	if len(res.Pending) > 0 {
		fmt.Printf("\n%d marked %s awaiting distillation (run /distill to generalize):\n",
			len(res.Pending), plural(len(res.Pending), "memory", "memories"))
		for _, o := range res.Pending {
			fmt.Printf("  - %s  (%s/%s)\n", o.Name, o.Project, o.File)
		}
	}
	if len(res.Conflicts) > 0 {
		fmt.Printf("\n%d %s — same name, divergent content (resolve in /distill):\n",
			len(res.Conflicts), plural(len(res.Conflicts), "conflict", "conflicts"))
		for _, c := range res.Conflicts {
			fmt.Printf("  - %s: %v\n", c.Name, c.Sources)
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
