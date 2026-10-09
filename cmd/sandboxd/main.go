// Command sandboxd runs untrusted commands in isolated Linux sandboxes behind
// an HTTP API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	goruntime "runtime"

	"github.com/Husqvarnalox/sandboxd/internal/config"
	"github.com/Husqvarnalox/sandboxd/internal/doctor"
)

// Set with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	// The sandbox init re-executes this binary inside fresh namespaces. It
	// must take over before any flag parsing, logging or other setup runs.
	if len(os.Args) > 1 && os.Args[1] == "internal-init" {
		runInit()
	}
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		cfg, code := parseConfig(args[1:])
		if cfg == nil {
			return code
		}
		if err := cfg.RequireRootfs(); err != nil {
			fmt.Fprintln(os.Stderr, "sandboxd:", err)
			return 2
		}
		return serve(cfg)
	case "doctor":
		cfg, code := parseConfig(args[1:])
		if cfg == nil {
			return code
		}
		checks := doctor.Run(cfg)
		doctor.Print(os.Stdout, checks)
		if doctor.Failed(checks) {
			return 1
		}
		return 0
	case "version":
		fmt.Printf("sandboxd %s (commit %s, %s)\n", version, commit, goruntime.Version())
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	}
	fmt.Fprintf(os.Stderr, "sandboxd: unknown command %q\n\n", args[0])
	usage(os.Stderr)
	return 2
}

func parseConfig(args []string) (*config.Config, int) {
	cfg, err := config.Parse(args, os.LookupEnv)
	if errors.Is(err, flag.ErrHelp) {
		usage(os.Stdout)
		return nil, 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd:", err)
		return nil, 2
	}
	return cfg, 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: sandboxd <command> [flags]

Commands:
  serve     run the HTTP API and execute jobs (Linux, root)
  doctor    check whether this host can run sandboxd (read-only)
  version   print version information
  help      show this help

Flags (serve and doctor):
`)
	config.PrintFlags(w)
}
