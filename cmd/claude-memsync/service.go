package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/kardianos/service"

	"github.com/MarimerLLC/claude-utils/internal/config"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// program implements the kardianos/service.Interface, hosting the sync
// loop inside the OS service supervisor.
type program struct {
	configPath string
	cancel     context.CancelFunc
	done       chan struct{}
	loopErr    error
}

func (p *program) Start(s service.Service) error {
	cfg, err := config.Load(p.configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", p.configPath, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		loop := &syncpkg.Loop{Cfg: cfg}
		p.loopErr = loop.Run(ctx)
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		<-p.done
	}
	return p.loopErr
}

// serviceConfig builds the kardianos/service Config used by every lifecycle
// subcommand. The OS-managed instance is always launched with the "run"
// argument so it shares a single code path with foreground use.
func serviceConfig(configPath string) *service.Config {
	args := []string{"run"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	return &service.Config{
		Name:        "claude-memsync",
		DisplayName: "Claude Memory Sync",
		Description: "Synchronizes Claude Code memories across workstations via a private git repository.",
		Arguments:   args,
	}
}

func newService(configPath string) (service.Service, *program, error) {
	prg := &program{configPath: configPath}
	svc, err := service.New(prg, serviceConfig(configPath))
	if err != nil {
		return nil, nil, err
	}
	return svc, prg, nil
}

func defaultConfigPath() string {
	return filepath.Join(config.Defaults().SyncDir, "config.json")
}

func runInstall(args []string) int {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "path to config.json (default: ~/.claudesync/config.json)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" {
		*cfgPath = defaultConfigPath()
	}
	abs, err := filepath.Abs(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve config path:", err)
		return 1
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(os.Stderr, "config %s not found — run `claude-memsync init` first\n", abs)
		return 1
	}
	svc, _, err := newService(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	if err := svc.Install(); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "installed; start with `claude-memsync start`")
	return 0
}

func runUninstall(args []string) int {
	svc, _, err := newService(defaultConfigPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	// Best-effort stop, then uninstall.
	_ = svc.Stop()
	if err := svc.Uninstall(); err != nil {
		fmt.Fprintln(os.Stderr, "uninstall:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "uninstalled")
	return 0
}

func runStart(args []string) int {
	svc, _, err := newService(defaultConfigPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	if err := svc.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "started")
	return 0
}

func runStop(args []string) int {
	svc, _, err := newService(defaultConfigPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	if err := svc.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "stopped")
	return 0
}

func runStatus(args []string) int {
	svc, _, err := newService(defaultConfigPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	st, err := svc.Status()
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		return 1
	}
	switch st {
	case service.StatusRunning:
		fmt.Println("running")
	case service.StatusStopped:
		fmt.Println("stopped")
	default:
		fmt.Println("unknown")
	}
	return 0
}

// runService dispatches to either foreground (`run`) or service-managed mode.
// kardianos/service detects the launching environment automatically.
func runService(configPath string) int {
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	svc, _, err := newService(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	logger, err := svc.Logger(nil)
	if err == nil {
		log.SetOutput(serviceLogWriter{logger: logger})
	}
	if err := svc.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	return 0
}

// serviceLogWriter adapts the standard log package to the kardianos service
// logger, so log output reaches the platform's native log destination
// (Windows Event Log, journald, etc.) when running under SCM.
type serviceLogWriter struct{ logger service.Logger }

func (w serviceLogWriter) Write(p []byte) (int, error) {
	_ = w.logger.Info(string(p))
	return len(p), nil
}
