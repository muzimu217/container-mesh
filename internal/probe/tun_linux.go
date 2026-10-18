//go:build linux

package probe

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// probeTUN checks TUN availability empirically: stat the node, open it, then
// issue TUNSETIFF. Reading capability bits alone is not enough because seccomp
// or an LSM can still deny the ioctl.
func probeTUN(opts Options) TUNReport {
	r := TUNReport{DevicePath: opts.TUNDevice}

	fi, err := os.Stat(opts.TUNDevice)
	if err != nil {
		r.DeviceExists = false
		r.OpenError = err.Error()
		return r
	}
	r.DeviceExists = true
	r.DeviceMode = fi.Mode().String()

	// Open read-write without blocking. A character device that does not
	// exist yields ENOENT here even if stat succeeded (e.g. a mount shadow).
	fd, err := unix.Open(opts.TUNDevice, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		r.OpenError = err.Error()
		return r
	}
	r.Openable = true
	defer unix.Close(fd)

	r.IoctlAttempted = true
	ifr, err := unix.NewIfreq(opts.IfaceName)
	if err != nil {
		r.IoctlError = fmt.Sprintf("ifreq: %v", err)
		return r
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		r.IoctlError = err.Error()
		return r
	}
	r.IoctlOK = true
	r.IfaceName = ifr.Name()
	r.Supported = true
	return r
}

// Compile-time guard: keep unsafe imported only where needed on linux.
var _ = unsafe.Sizeof(0)
