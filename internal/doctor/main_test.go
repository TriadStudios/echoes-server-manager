package doctor

import (
	"os"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// testkit.Run fails the run if a test made ESM bind an address that is not
// loopback.
func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }
