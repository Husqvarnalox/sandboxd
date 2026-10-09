// Package doctor runs read-only host diagnostics for `sandboxd doctor`. It
// never creates, modifies or removes anything.
package doctor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/Husqvarnalox/sandboxd/internal/config"
)

type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is the result of one diagnostic.
type Check struct {
	Name   string
	Status Status
	Detail string
}

// Failed reports whether any check failed.
func Failed(checks []Check) bool {
	for _, c := range checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Print writes checks as an aligned table.
func Print(w io.Writer, checks []Check) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range checks {
		fmt.Fprintf(tw, "[%s]\t%s\t%s\n", c.Status, c.Name, c.Detail)
	}
	tw.Flush()
}

func rootfsChecks(cfg *config.Config) []Check {
	if cfg.Rootfs == "" {
		return []Check{{"rootfs", Fail, "--rootfs is not set"}}
	}
	fi, err := os.Stat(cfg.Rootfs)
	if err != nil || !fi.IsDir() {
		return []Check{{"rootfs", Fail, fmt.Sprintf("%s is not a directory", cfg.Rootfs)}}
	}
	checks := []Check{{"rootfs", OK, cfg.Rootfs}}
	if fi, err := os.Stat(filepath.Join(cfg.Rootfs, "bin", "sh")); err != nil || fi.IsDir() {
		checks = append(checks, Check{"rootfs bin/sh", Fail, "missing bin/sh in rootfs"})
	} else {
		checks = append(checks, Check{"rootfs bin/sh", OK, "present"})
	}
	var missing []string
	for _, d := range []string{"proc", "dev", "tmp", "workspace"} {
		if fi, err := os.Stat(filepath.Join(cfg.Rootfs, d)); err != nil || !fi.IsDir() {
			missing = append(missing, d+"/")
		}
	}
	if len(missing) > 0 {
		checks = append(checks, Check{"rootfs mount points", Warn,
			fmt.Sprintf("missing %v; created at job start only if the rootfs is writable", missing)})
	} else {
		checks = append(checks, Check{"rootfs mount points", OK, "proc/ dev/ tmp/ workspace/ present"})
	}
	return checks
}

func executableCheck() Check {
	exe, err := os.Executable()
	if err != nil {
		return Check{"executable", Fail, fmt.Sprintf("cannot resolve own binary (needed for sandbox init): %v", err)}
	}
	return Check{"executable", OK, exe}
}
