//go:build !linux

package main

import (
	"errors"
	"log/slog"

	"github.com/Husqvarnalox/sandboxd/internal/config"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
)

func newRuntime(*config.Config, *slog.Logger) (runtime.Runtime, error) {
	return nil, errors.New("sandboxd can only run jobs on Linux; see README")
}
