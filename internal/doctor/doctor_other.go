//go:build !linux

package doctor

import (
	"fmt"
	"os"
	goruntime "runtime"

	"github.com/Husqvarnalox/sandboxd/internal/config"
)

// Run performs the diagnostics that make sense off Linux.
func Run(cfg *config.Config) []Check {
	checks := []Check{{"platform", Fail, fmt.Sprintf("sandboxd runs jobs only on Linux (this is %s)", goruntime.GOOS)}}
	checks = append(checks, rootfsChecks(cfg)...)
	if fi, err := os.Stat(cfg.StateDir); err != nil || !fi.IsDir() {
		checks = append(checks, Check{"state dir", Warn, cfg.StateDir + " does not exist; serve will create it"})
	} else {
		checks = append(checks, Check{"state dir", OK, cfg.StateDir})
	}
	return append(checks, executableCheck())
}
