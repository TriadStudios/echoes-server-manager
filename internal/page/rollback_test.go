package page_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/release"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
)

// When a self-update did not come up and the old binary was put back, the Box
// screen says so in a notice of the usual kind, for every role, with the
// versions and when it happened. A box where that did not happen has no
// such notice.
func TestBoxShowsARolledBackSelfUpdate(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "esm-under-test")
	tune := func(o *service.Options) { o.Executable = exe }

	plain := newSite(t, tune)
	plain.setup()
	if b := plain.checkPage("/box", "session", plain.csrf, plain.session); strings.Contains(b, "did not come up") {
		t.Errorf("the Box screen of a box with no rollback has a notice:\n%s", b)
	}

	rec := `{"from":"v1.0.0","to":"v1.1.0","time":"2026-11-16T10:00:00Z","rolled_back_at":"2026-11-16T10:20:30Z"}` + "\n"
	if err := os.WriteFile(exe+release.RolledBackSuffix, []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSite(t, tune)
	s.setup()
	read := s.token("read")
	for who, b := range map[string]string{
		"admin": s.checkPage("/box", "session", s.csrf, s.session),
		"read":  s.checkPage("/box", read, "", read),
	} {
		for _, want := range []string{
			`<p class="alert warn"><strong class="tag">Warning</strong> The update to ESM v1.1.0 did not come up, so the old binary was put back on <time datetime="2026-11-16T10:20:30Z">`,
			`: this is ESM v1.0.0 again. The binary that failed is kept next to ESM as <span class="mono">.failed</span>`,
		} {
			if !strings.Contains(b, want) {
				t.Errorf("the Box screen of %s lacks %q", who, want)
			}
		}
	}

	// A record with no versions still gets its notice.
	os.WriteFile(exe+release.RolledBackSuffix, []byte(`{"rolled_back_at":"2026-11-16T10:20:30Z"}`+"\n"), 0o644)
	bare := newSite(t, tune)
	bare.setup()
	if b := bare.checkPage("/box", "session", bare.csrf, bare.session); !strings.Contains(b, "A self-update did not come up, so the old binary was put back on <time") ||
		strings.Contains(b, "this is ESM") {
		t.Errorf("the Box screen with a record that names no versions:\n%s", b)
	}
}
