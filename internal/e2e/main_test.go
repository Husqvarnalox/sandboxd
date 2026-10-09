//go:build linux

package e2e

import (
	"os"
	"testing"

	"github.com/Husqvarnalox/sandboxd/internal/runtime/linux"
)

// TestMain lets the test binary double as the sandbox init: the runtime
// re-executes InitPath (a copy of this binary) with linux.InitArg.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == linux.InitArg {
		linux.RunInit()
	}
	os.Exit(m.Run())
}
