package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kardianos/service"

	"github.com/MarimerLLC/claude-utils/internal/config"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

// program implements kardianos/service.Interface so that the `run`
// subcommand works identically when launched from a terminal, by a Windows
// scheduled task, or by a systemd/launchd user unit. The lifecycle install
// path (install/uninstall/start/stop/status) is handled separately per
// platform — see task_windows.go and task_other.go.
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

func defaultConfigPath() string {
	return filepath.Join(defaultSyncDir(), "config.json")
}

func defaultSyncDir() string {
	return config.Defaults().SyncDir
}

// pidFilePath is the location of the daemon PID file. It is written by
// runService at startup (and removed on exit) so that the start/stop/status
// subcommands on Windows can manage the daemon process directly.
func pidFilePath() string {
	return filepath.Join(defaultSyncDir(), "daemon.pid")
}

func readPidFile() (int, error) {
	b, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func writePidFile(pid int) error {
	if err := os.MkdirAll(filepath.Dir(pidFilePath()), 0700); err != nil {
		return err
	}
	return os.WriteFile(pidFilePath(), []byte(strconv.Itoa(pid)), 0600)
}

func removePidFile() error {
	if err := os.Remove(pidFilePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
	if err := installTask(abs); err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "installed; start with `claude-memsync start`")
	return 0
}

func runUninstall(args []string) int {
	if err := uninstallTask(); err != nil {
		fmt.Fprintln(os.Stderr, "uninstall:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "uninstalled")
	return 0
}

func runStart(args []string) int {
	if err := startTask(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "started")
	return 0
}

func runStop(args []string) int {
	if err := stopTask(); err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "stopped")
	return 0
}

func runStatus(args []string) int {
	st, err := statusTask()
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		return 1
	}
	fmt.Println(st)
	return 0
}

// runService is the entry point for `claude-memsync run`. It wraps the loop
// in kardianos's service.Run() so the same code path serves both interactive
// invocation (terminal, Windows scheduled task, systemd user unit) and any
// future move to true SCM-managed mode.
func runService(configPath string) int {
	if configPath == "" {
		configPath = defaultConfigPath()
	}

	if err := writePidFile(os.Getpid()); err != nil {
		log.Println("warning: write pid file:", err)
	}
	defer removePidFile()

	prg := &program{configPath: configPath}
	svc, err := service.New(prg, serviceConfig(configPath))
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

type serviceLogWriter struct{ logger service.Logger }

func (w serviceLogWriter) Write(p []byte) (int, error) {
	_ = w.logger.Info(string(p))
	return len(p), nil
}
