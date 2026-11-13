package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"testing"
)

// A release whose signed SHA256SUMS belongs to another version is refused,
// whatever tag the (unsigned) release list puts on it: an old signed
// release served as a newer one does not install.
func TestUpdateRefusesReplayedRelease(t *testing.T) {
	pub, priv := testKey(t)
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	bin := []byte("the old, signed v1.0.0 binary")
	h := sha256.Sum256(bin)
	for what, sums := range map[string]string{
		"no version line":      fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), name),
		"signed for v1.0.0":    fmt.Sprintf("version v1.0.0\n%s  %s\n", hex.EncodeToString(h[:]), name),
		"signed for v1.0.9":    fmt.Sprintf("version v1.0.9\n%s  %s\n", hex.EncodeToString(h[:]), name),
		"version line twice":   fmt.Sprintf("version v1.1.0\nversion v1.1.0\n%s  %s\n", hex.EncodeToString(h[:]), name),
		"version line not top": fmt.Sprintf("%s  %s\nversion v1.1.0\n", hex.EncodeToString(h[:]), name),
	} {
		t.Run(what, func(t *testing.T) {
			fr := &fakeRelease{tag: "v1.1.0", files: map[string][]byte{name: bin, SumsName: []byte(sums), SigName: Sign(priv, []byte(sums))}}
			_, c := fr.start(t)
			c.Key = pub
			refused(t, c, SumsName)
		})
	}
}

// The signed version must also be newer than the running one, unless
// forced: an old release under its own tag is not taken without force.
func TestUpdateSignedVersionMustBeNewer(t *testing.T) {
	pub, priv := testKey(t)
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	bin := []byte("v0.9.0 binary")
	h := sha256.Sum256(bin)
	sums := fmt.Sprintf("version v0.9.0\n%s  %s\n", hex.EncodeToString(h[:]), name)
	fr := &fakeRelease{tag: "v0.9.0", files: map[string][]byte{name: bin, SumsName: []byte(sums), SigName: Sign(priv, []byte(sums))}}
	_, c := fr.start(t)
	c.Key = pub
	exe := oldBinary(t)
	if _, err := c.Update(t.Context(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false); err == nil {
		t.Fatal("an older release was taken without force")
	}
	if _, err := c.Update(t.Context(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, true); err != nil {
		t.Fatalf("with force: %v", err)
	}
}
