//go:build !linux && !windows

package testkit

func processSockets(int) ([]Socket, error) { return nil, ErrSocketsUnsupported }
