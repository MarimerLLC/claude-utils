//go:build !windows

package main

// On Linux and macOS we use kardianos/service with the UserService option,
// which generates a systemd user unit (~/.config/systemd/user/) on Linux
// and a launchd plist (~/Library/LaunchAgents/) on macOS. The unit/plist
// runs in the user's session — full access to their credentials and SSH
// keys — and can be installed/started without root.

import (
	"github.com/kardianos/service"
)

func userSvc(configPath string) (service.Service, error) {
	if configPath == "" {
		configPath = defaultConfigPath()
	}
	cfg := serviceConfig(configPath)
	if cfg.Option == nil {
		cfg.Option = service.KeyValue{}
	}
	cfg.Option["UserService"] = true
	prg := &program{configPath: configPath}
	return service.New(prg, cfg)
}

func installTask(configPath string) error {
	svc, err := userSvc(configPath)
	if err != nil {
		return err
	}
	return svc.Install()
}

func uninstallTask() error {
	svc, err := userSvc("")
	if err != nil {
		return err
	}
	_ = svc.Stop()
	return svc.Uninstall()
}

func startTask() error {
	svc, err := userSvc("")
	if err != nil {
		return err
	}
	return svc.Start()
}

func stopTask() error {
	svc, err := userSvc("")
	if err != nil {
		return err
	}
	return svc.Stop()
}

func statusTask() (string, error) {
	svc, err := userSvc("")
	if err != nil {
		return "unknown", err
	}
	st, err := svc.Status()
	if err != nil {
		return "unknown", err
	}
	switch st {
	case service.StatusRunning:
		return "running", nil
	case service.StatusStopped:
		return "stopped", nil
	default:
		return "unknown", nil
	}
}
