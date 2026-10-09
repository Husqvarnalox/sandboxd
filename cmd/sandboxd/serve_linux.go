//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/Husqvarnalox/sandboxd/internal/cgroup"
	"github.com/Husqvarnalox/sandboxd/internal/config"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
	"github.com/Husqvarnalox/sandboxd/internal/runtime/linux"
)

func newRuntime(cfg *config.Config, log *slog.Logger) (runtime.Runtime, error) {
	tree, err := cgroup.Open(cfg.CgroupRoot, cfg.CgroupName)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve own executable for sandbox init: %w", err)
	}
	return linux.New(linux.Config{
		Rootfs:      cfg.Rootfs,
		Tree:        tree,
		InitPath:    exe,
		UserNS:      cfg.UserNS,
		HostUIDBase: cfg.HostUIDBase,
		SandboxUID:  cfg.SandboxUID,
		SandboxGID:  cfg.SandboxUID,
		Logger:      log,
	})
}
