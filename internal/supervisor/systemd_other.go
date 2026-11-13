//go:build !linux

package supervisor

import (
	"errors"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// SystemdAvailable is always false off Linux.
func SystemdAvailable() bool { return false }

// NewSystemd is only available on Linux.
func NewSystemd(*store.Store, Options) (Supervisor, error) {
	return nil, errors.New("supervisor: systemd is only available on Linux")
}
