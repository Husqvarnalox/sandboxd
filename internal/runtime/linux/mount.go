//go:build linux

package linux

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// setupRootfs builds the sandbox's filesystem view inside init's private
// mount namespace and pivots into it. Nothing here is visible to the host,
// so there is nothing to clean up when the namespace dies.
func setupRootfs(cfg *initConfig) error {
	root := cfg.Rootfs
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	// pivot_root needs the new root to be a mount point.
	if err := unix.Mount(root, root, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs %s: %w", root, err)
	}
	// The mount points normally ship with the rootfs (scripts/build-rootfs.sh
	// creates them). Creating them here is best effort: init runs as the
	// mapped sandbox uid, so a root-owned or read-only rootfs yields EACCES
	// or EROFS. Only a mount point that is still missing afterwards is fatal.
	for _, d := range []string{"proc", "dev", "tmp", "workspace"} {
		p := filepath.Join(root, d)
		_ = os.MkdirAll(p, 0o755)
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return fmt.Errorf("rootfs has no %s directory (run scripts/build-rootfs.sh or create it)", d)
		}
	}

	if err := mount("proc", filepath.Join(root, "proc"), "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return err
	}
	if err := setupDev(filepath.Join(root, "dev")); err != nil {
		return err
	}
	if err := mount("tmpfs", filepath.Join(root, "tmp"), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=256m"); err != nil {
		return err
	}

	ws := filepath.Join(root, "workspace")
	if err := mount(cfg.Workspace, ws, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err := remount(ws, unix.MS_NOSUID|unix.MS_NODEV); err != nil {
		return err
	}
	// Only the rootfs bind itself becomes read-only; /proc, /dev, /tmp and
	// /workspace are separate mounts and keep their own flags.
	if err := remount(root, unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV); err != nil {
		return err
	}

	// The runc trick: pivot onto the rootfs with the old root stacked on top
	// of it, then lazily detach the old root. No put_old directory needed.
	if err := unix.Chdir(root); err != nil {
		return fmt.Errorf("chdir rootfs: %w", err)
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	return nil
}

func setupDev(dev string) error {
	if err := mount("tmpfs", dev, "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=755,size=64k"); err != nil {
		return err
	}
	// Device nodes cannot be created inside a user namespace; bind the
	// host's harmless ones instead.
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		src := "/dev/" + name
		if _, err := os.Stat(src); err != nil {
			if name == "tty" {
				continue
			}
			return fmt.Errorf("host device %s: %w", src, err)
		}
		dst := filepath.Join(dev, name)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return fmt.Errorf("create %s: %w", dst, err)
		}
		_ = f.Close()
		if err := mount(src, dst, "", unix.MS_BIND, ""); err != nil {
			return err
		}
	}
	for link, target := range map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
	} {
		if err := os.Symlink(target, filepath.Join(dev, link)); err != nil {
			return fmt.Errorf("create /dev/%s: %w", link, err)
		}
	}
	shm := filepath.Join(dev, "shm")
	if err := os.Mkdir(shm, 0o755); err != nil {
		return fmt.Errorf("create /dev/shm: %w", err)
	}
	return mount("tmpfs", shm, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=64m")
}

func mount(src, dst, fstype string, flags uintptr, data string) error {
	if err := unix.Mount(src, dst, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s: %w", src, dst, err)
	}
	return nil
}

// remount applies flags to the bind mount at path while keeping the flags the
// mount already has. Inside a user namespace, flags inherited from a more
// privileged mount namespace are locked; omitting one makes the remount fail
// with EPERM.
func remount(path string, flags uintptr) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}
	for _, m := range []struct{ st, ms uintptr }{
		{unix.ST_RDONLY, unix.MS_RDONLY},
		{unix.ST_NOSUID, unix.MS_NOSUID},
		{unix.ST_NODEV, unix.MS_NODEV},
		{unix.ST_NOEXEC, unix.MS_NOEXEC},
		{unix.ST_SYNCHRONOUS, unix.MS_SYNCHRONOUS},
		{unix.ST_MANDLOCK, unix.MS_MANDLOCK},
		{unix.ST_NOATIME, unix.MS_NOATIME},
		{unix.ST_NODIRATIME, unix.MS_NODIRATIME},
		{unix.ST_RELATIME, unix.MS_RELATIME},
	} {
		if uintptr(st.Flags)&m.st != 0 {
			flags |= m.ms
		}
	}
	if err := unix.Mount("", path, "", unix.MS_BIND|unix.MS_REMOUNT|flags, ""); err != nil {
		return fmt.Errorf("remount %s: %w", path, err)
	}
	return nil
}

// loopbackUp brings up lo in the job's otherwise empty network namespace so
// workloads can use 127.0.0.1; there is no route to anything else.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("loopback socket: %w", err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("loopback ifreq: %w", err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("get lo flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bring lo up: %w", err)
	}
	return nil
}
