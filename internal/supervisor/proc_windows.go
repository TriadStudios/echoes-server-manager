package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	processSupported = true
	// signalsAvailable is false: the polite stop is the stop request file.
	signalsAvailable = false

	createNewProcessGroup          = 0x00000200
	detachedProcess                = 0x00000008
	createNoWindow                 = 0x08000000
	processQueryLimitedInformation = 0x1000
)

var (
	modkernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procQueryFullProcessImageNameW = modkernel32.NewProc("QueryFullProcessImageNameW")
)

// detachedAttr starts the child without a console, in its own process
// group, so it survives ESM and its console events.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
	}
}

// textBusy is Linux's "text file busy"; Windows reports a sharing
// violation instead, which is not waited out.
func textBusy(error) bool { return false }

func interrupt(*os.Process) error {
	return errors.New("signals are not available on Windows")
}

func openQuery(pid int) (syscall.Handle, error) {
	return syscall.OpenProcess(processQueryLimitedInformation|syscall.SYNCHRONIZE, false, uint32(pid))
}

func creationMarker(h syscall.Handle) (string, error) {
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("windows:%d", uint64(created.HighDateTime)<<32|uint64(created.LowDateTime)), nil
}

// startMarker returns the creation time of pid as a marker.
func startMarker(pid int) (string, error) {
	h, err := openQuery(pid)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(h)
	return creationMarker(h)
}

// inspect reports whether pid is alive and its start marker.
func inspect(pid int) (alive bool, marker string, err error) {
	h, err := openQuery(pid)
	if err != nil {
		// No such process (or one we may not look at): either way it is
		// not something ESM can claim.
		return false, "", nil
	}
	defer syscall.CloseHandle(h)
	ev, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return false, "", err
	}
	if ev == syscall.WAIT_OBJECT_0 {
		return false, "", nil // exited; the object lingers while handles exist
	}
	m, err := creationMarker(h)
	if err != nil {
		return false, "", err
	}
	return true, m, nil
}

func imagePath(pid int) (string, error) {
	h, err := openQuery(pid)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	r1, _, e1 := procQueryFullProcessImageNameW.Call(uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r1 == 0 {
		return "", e1
	}
	return syscall.UTF16ToString(buf[:size]), nil
}

// sameExecutable reports whether pid runs the file at path.
func sameExecutable(pid int, path string) bool {
	img, err := imagePath(pid)
	if err != nil {
		return false
	}
	a, errA := os.Stat(img)
	b, errB := os.Stat(path)
	if errA == nil && errB == nil {
		return os.SameFile(a, b)
	}
	return strings.EqualFold(filepath.Clean(img), filepath.Clean(path))
}
