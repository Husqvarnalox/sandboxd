//go:build linux

package linux

import (
	"os"
	"testing"
)

// TestMain lets the test binary double as the sandbox init (Config.InitPath
// is os.Executable()) and as the seccomp test helper.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == InitArg {
		RunInit()
	}
	if os.Getenv(seccompHelperEnv) != "" {
		seccompHelper()
	}
	os.Exit(m.Run())
}
