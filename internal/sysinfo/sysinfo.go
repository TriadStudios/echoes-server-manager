// Package sysinfo reads what ESM can learn cheaply about the box it runs on
// and about one server process: memory, CPU count and load, disk space.
//
// Every value is read from the operating system when asked; nothing is
// estimated. A value this system cannot give is left out (nil), and the
// reason is named in Unknown, so whoever shows it can say why.
package sysinfo

import "runtime"

// Unknown names a value that could not be read, and why.
type Unknown struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Box is what the box has and uses right now. Sizes are in bytes.
type Box struct {
	// MemoryTotalBytes is the physical memory of the box.
	MemoryTotalBytes *int64 `json:"memory_total_bytes"`
	// MemoryAvailableBytes is the memory that new programs can still get
	// without swapping (Linux MemAvailable; Windows available physical
	// memory).
	MemoryAvailableBytes *int64 `json:"memory_available_bytes"`
	// CPUCount is the number of logical CPUs.
	CPUCount int `json:"cpu_count"`
	// Load1 is the load average over the last minute (Linux).
	Load1 *float64 `json:"load_1m"`
	// DiskTotalBytes and DiskFreeBytes describe the file system that holds
	// the ESM root; free is what this user may still write.
	DiskTotalBytes *int64 `json:"disk_total_bytes"`
	DiskFreeBytes  *int64 `json:"disk_free_bytes"`
	// Unknown lists the values above that could not be read. Never nil.
	Unknown []Unknown `json:"unknown"`
}

// ReadBox reads the box's memory, CPUs, load and the disk space of the file
// system that holds dir.
func ReadBox(dir string) Box {
	b := Box{CPUCount: runtime.NumCPU(), Unknown: []Unknown{}}
	unknown := func(field string, err error) {
		b.Unknown = append(b.Unknown, Unknown{Field: field, Reason: err.Error()})
	}
	if total, avail, err := memory(); err != nil {
		unknown("memory_total_bytes", err)
		unknown("memory_available_bytes", err)
	} else {
		b.MemoryTotalBytes, b.MemoryAvailableBytes = &total, &avail
	}
	if l, err := load1(); err != nil {
		unknown("load_1m", err)
	} else {
		b.Load1 = &l
	}
	if total, free, err := DiskSpace(dir); err != nil {
		unknown("disk_total_bytes", err)
		unknown("disk_free_bytes", err)
	} else {
		t, f := int64(total), int64(free)
		b.DiskTotalBytes, b.DiskFreeBytes = &t, &f
	}
	return b
}

// Process memory sources.
const (
	// SourceCgroup is the memory of the server's own systemd unit (its
	// cgroup's memory.current): the server and everything it started.
	SourceCgroup = "cgroup"
	// SourceProcess is the resident memory of the server process alone.
	SourceProcess = "process"
)

// ProcessMemory reads the memory a server process uses. When the process
// runs in its own cgroup whose last path element is unit (a systemd unit
// such as "esm-alpha.service"), that cgroup's memory is read: it counts
// the server and its children. Otherwise it is the resident memory of the
// process. pid must be the server's process.
func ProcessMemory(pid int, unit string) (bytes int64, source string, err error) {
	return processMemory(pid, unit)
}
