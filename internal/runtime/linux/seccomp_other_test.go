//go:build linux && !amd64 && !arm64

package linux

const seccompHelperEnv = "SANDBOXD_TEST_SECCOMP_HELPER"

func seccompHelper() {}
