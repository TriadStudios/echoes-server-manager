package sysinfo

import (
	"os"
	"runtime"
	"testing"
)

// Every value is either read or named in Unknown with a reason, never both
// and never neither.
func TestReadBoxIsHonest(t *testing.T) {
	b := ReadBox(t.TempDir())
	if b.CPUCount < 1 || b.CPUCount != runtime.NumCPU() {
		t.Fatalf("cpu count %d", b.CPUCount)
	}
	if b.Unknown == nil {
		t.Fatal("Unknown is nil")
	}
	unknown := map[string]string{}
	for _, u := range b.Unknown {
		if u.Reason == "" {
			t.Errorf("%s is unknown without a reason", u.Field)
		}
		unknown[u.Field] = u.Reason
	}
	fields := map[string]bool{
		"memory_total_bytes":     b.MemoryTotalBytes != nil,
		"memory_available_bytes": b.MemoryAvailableBytes != nil,
		"load_1m":                b.Load1 != nil,
		"disk_total_bytes":       b.DiskTotalBytes != nil,
		"disk_free_bytes":        b.DiskFreeBytes != nil,
	}
	for f, known := range fields {
		_, listed := unknown[f]
		if known == listed {
			t.Errorf("%s: known %v, listed as unknown %v", f, known, listed)
		}
	}
	if b.MemoryTotalBytes != nil && (*b.MemoryTotalBytes <= 0 || *b.MemoryAvailableBytes < 0 || *b.MemoryAvailableBytes > *b.MemoryTotalBytes) {
		t.Errorf("memory %d of %d available", *b.MemoryAvailableBytes, *b.MemoryTotalBytes)
	}
	if b.DiskTotalBytes != nil && (*b.DiskTotalBytes <= 0 || *b.DiskFreeBytes < 0 || *b.DiskFreeBytes > *b.DiskTotalBytes) {
		t.Errorf("disk %d of %d free", *b.DiskFreeBytes, *b.DiskTotalBytes)
	}
	switch runtime.GOOS {
	case "linux":
		for _, f := range []string{"memory_total_bytes", "load_1m", "disk_total_bytes"} {
			if !fields[f] {
				t.Errorf("%s not read on Linux: %s", f, unknown[f])
			}
		}
	case "windows":
		if !fields["memory_total_bytes"] || !fields["disk_total_bytes"] || fields["load_1m"] {
			t.Errorf("windows: %+v", b)
		}
	}
}

func TestDiskSpaceOfMissingFolder(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("not read on this system")
	}
	if _, _, err := DiskSpace(t.TempDir() + string(os.PathSeparator) + "missing"); err == nil {
		t.Fatal("no error for a folder that does not exist")
	}
}

func TestProcessMemoryOfThisProcess(t *testing.T) {
	n, src, err := ProcessMemory(os.Getpid(), "")
	switch runtime.GOOS {
	case "linux":
		if err != nil || n <= 0 || src != SourceProcess {
			t.Fatalf("%d %q %v", n, src, err)
		}
	default:
		if err == nil || err.Error() == "" {
			t.Fatalf("%s: memory read without support: %d", runtime.GOOS, n)
		}
	}
	if _, _, err := ProcessMemory(0, ""); err == nil {
		t.Fatal("pid 0 read")
	}
}
