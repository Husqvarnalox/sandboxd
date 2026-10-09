//go:build linux && !amd64 && !arm64

package linux

import (
	"errors"

	"golang.org/x/sys/unix"
)

func seccompFilter() (*unix.SockFprog, error) {
	return nil, errors.New("seccomp filter: unsupported architecture")
}

func installSeccomp() error {
	_, err := seccompFilter()
	return err
}
