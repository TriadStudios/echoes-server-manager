//go:build !linux && !windows

package sysinfo

import "errors"

var errUnsupported = errors.New("ESM does not read this on this system")

func memory() (total, avail int64, err error) { return 0, 0, errUnsupported }

func load1() (float64, error) { return 0, errUnsupported }

// DiskSpace is not supported on this system.
func DiskSpace(string) (total, free uint64, err error) { return 0, 0, errUnsupported }

func processMemory(int, string) (int64, string, error) { return 0, "", errUnsupported }
