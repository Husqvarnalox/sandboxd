//go:build linux

package main

import "github.com/Husqvarnalox/sandboxd/internal/runtime/linux"

func runInit() { linux.RunInit() }
