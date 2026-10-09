//go:build !linux

package main

import (
	"fmt"
	"os"
)

func runInit() {
	fmt.Fprintln(os.Stderr, "sandboxd: internal-init is only available on Linux")
	os.Exit(1)
}
