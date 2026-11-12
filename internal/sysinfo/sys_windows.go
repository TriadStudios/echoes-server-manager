package sysinfo

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func memory() (total, avail int64, err error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r1, _, e1 := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r1 == 0 {
		return 0, 0, e1
	}
	return int64(m.TotalPhys), int64(m.AvailPhys), nil
}

func load1() (float64, error) {
	return 0, errors.New("Windows has no load average, and ESM does not sample CPU use there yet")
}

// DiskSpace returns the size of the file system that holds dir and the
// bytes this user may still write there.
func DiskSpace(dir string) (total, free uint64, err error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var avail, tot, all uint64
	r1, _, e1 := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&tot)), uintptr(unsafe.Pointer(&all)))
	if r1 == 0 {
		return 0, 0, e1
	}
	return tot, avail, nil
}

func processMemory(int, string) (int64, string, error) {
	return 0, "", errors.New("ESM does not measure server memory on Windows yet")
}
