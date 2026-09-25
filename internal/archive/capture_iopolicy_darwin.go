//go:build darwin

package archive

import (
	"runtime"
	"syscall"
	"unsafe"
)

// setiopolicy_np lives in libc, which Go does not link; it is a thin wrapper
// over the iopolicysys system call, whose ABI (verified against libc on macOS
// 15) is below.
const (
	sysIOPolicy     = 322 // SYS_IOPOLICYSYS
	ioPolicyGet     = 1
	ioPolicySet     = 2
	ioPolicyDisk    = 0 // IOPOL_TYPE_DISK
	ioPolicyThread  = 1 // IOPOL_SCOPE_THREAD
	ioPolicyUtility = 4 // IOPOL_UTILITY
)

type ioPolicyParam struct{ scope, iotype, policy int32 }

func ioPolicy(command int, param *ioPolicyParam) error {
	if _, _, errno := syscall.Syscall(sysIOPolicy, uintptr(command), uintptr(unsafe.Pointer(param)), 0); errno != 0 {
		return errno
	}
	return nil
}

// currentIOPolicy reports the calling thread's disk I/O policy.
func currentIOPolicy() (int32, error) {
	param := ioPolicyParam{scope: ioPolicyThread, iotype: ioPolicyDisk}
	err := ioPolicy(ioPolicyGet, &param)
	return param.policy, err
}

// withCaptureIOPolicy runs fn with its disk I/O in the utility tier, which
// yields to the interactive work of whoever is using the Mac but, unlike the
// background (throttle) tier, still finishes promptly when someone is waiting
// to unplug. The policy is per thread so the service's own requests keep their
// priority; fn must do its I/O on the calling goroutine.
func withCaptureIOPolicy(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	previous, err := currentIOPolicy()
	if err == nil && ioPolicy(ioPolicySet, &ioPolicyParam{scope: ioPolicyThread, iotype: ioPolicyDisk, policy: ioPolicyUtility}) == nil {
		// The locked thread returns to the pool afterwards; restore it first.
		defer ioPolicy(ioPolicySet, &ioPolicyParam{scope: ioPolicyThread, iotype: ioPolicyDisk, policy: previous})
	}
	fn()
}
