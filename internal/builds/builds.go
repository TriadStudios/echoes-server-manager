// Package builds installs and keeps game server builds under
// <root>/builds/<version>/.
//
// A build is a folder with a manifest.json that lists every file with its
// SHA-256 and names the executable for each operating system. A build is
// installed from a local folder (copied), a local .zip, .tar.gz or .tgz
// archive (unpacked with path checks), or an https URL with a required
// SHA-256 (downloaded, checked, then unpacked). Everything is first placed
// in <root>/tmp/, the manifest and every listed hash are checked, and only
// then is the folder moved into builds/. Installed files are read-only.
package builds

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// Folders inside the ESM root.
const (
	Dir     = "builds"
	infoDir = "builds/.info"
	tempDir = "tmp"
	// tempPrefix marks install work folders in tmp/.
	tempPrefix = "build-"
)

// Limits bound what one install may unpack or download.
type Limits struct {
	MaxFiles int   // default 200000
	MaxBytes int64 // total unpacked size and download size; default 128 GiB
}

// Options configure a Manager.
type Options struct {
	GOOS       string       // default runtime.GOOS
	Limits     Limits       // zero fields take the defaults
	HTTPClient *http.Client // for https sources; default: no overall timeout, https-only redirects
	Bus        *events.Bus  // optional: build events are published here
	Logger     *slog.Logger
}

// Build describes an installed build.
type Build struct {
	Version    string    `json:"version"`
	Commit     string    `json:"commit"`
	Channel    string    `json:"channel"`
	Executable string    `json:"executable"` // slash path inside the build for this OS
	Installed  time.Time `json:"installed"`
	Files      int       `json:"files"`
	SizeBytes  int64     `json:"size_bytes"`
}

// info is the sidecar builds/.info/<version>.json.
type info struct {
	Installed time.Time `json:"installed"`
	Files     int       `json:"files"`
	SizeBytes int64     `json:"size_bytes"`
}

// Event is the data of a "build" event.
type Event struct {
	Action  string `json:"action"` // installed | removed
	Version string `json:"version"`
}

// Manager installs, lists and removes builds.
type Manager struct {
	st   *store.Store
	opts Options
	log  *slog.Logger
	mu   sync.Mutex // serializes moving builds in and out of builds/
}

// New returns a Manager for the store root.
func New(st *store.Store, opts Options) *Manager {
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.Limits.MaxFiles <= 0 {
		opts.Limits.MaxFiles = 200000
	}
	if opts.Limits.MaxBytes <= 0 {
		opts.Limits.MaxBytes = 128 << 30
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       30 * time.Second,
			},
			CheckRedirect: httpsOnlyRedirect,
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Manager{st: st, opts: opts, log: opts.Logger}
}

func httpsOnlyRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("redirect to a non-https URL refused")
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

// CleanTemp removes install work folders left in tmp/ by an interrupted
// install. Call it once at start, before any install runs.
func (m *Manager) CleanTemp() error {
	entries, err := m.st.ReadDir(tempDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			if err := m.st.RemoveAll(tempDir + "/" + e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

func randomName() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func buildDir(version string) string { return Dir + "/" + version }

// Get returns an installed build.
func (m *Manager) Get(version string) (Build, error) {
	if !ValidVersion(version) {
		return Build{}, fault.NotFoundf("build %q not found", version)
	}
	b, err := m.st.ReadFile(buildDir(version) + "/" + ManifestName)
	if errors.Is(err, fs.ErrNotExist) {
		return Build{}, fault.NotFoundf("build %q not found", version)
	}
	if err != nil {
		return Build{}, err
	}
	man, err := ParseManifest(b)
	if err != nil {
		return Build{}, err
	}
	out := Build{Version: version, Commit: man.Commit, Channel: man.Channel,
		Executable: man.Executables[m.opts.GOOS], Files: len(man.Files) + 1}
	var in info
	if err := m.st.ReadJSON(infoDir+"/"+version+".json", &in); err == nil {
		out.Installed, out.Files, out.SizeBytes = in.Installed, in.Files, in.SizeBytes
	} else if fi, err := m.st.Stat(buildDir(version) + "/" + ManifestName); err == nil {
		out.Installed = fi.ModTime().UTC()
	}
	return out, nil
}

// ExecutablePath returns the absolute path of the executable of a build
// for this operating system.
func (m *Manager) ExecutablePath(version string) (string, error) {
	b, err := m.Get(version)
	if err != nil {
		return "", err
	}
	if b.Executable == "" {
		return "", fault.New(fault.Invalid, "no_executable", "build %s has no executable for %s", version, m.opts.GOOS)
	}
	return m.st.Abs(buildDir(version) + "/" + b.Executable)
}

// List returns the installed builds, newest first. Folders that are not
// valid builds are skipped.
func (m *Manager) List() ([]Build, error) {
	entries, err := m.st.ReadDir(Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Build{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Build{}
	for _, e := range entries {
		if !e.IsDir() || !ValidVersion(e.Name()) {
			continue
		}
		b, err := m.Get(e.Name())
		if err != nil {
			m.log.Warn("skipping unreadable build", "version", e.Name(), "err", err)
			continue
		}
		out = append(out, b)
	}
	slices.SortStableFunc(out, func(a, b Build) int {
		if c := b.Installed.Compare(a.Installed); c != 0 {
			return c
		}
		return strings.Compare(a.Version, b.Version)
	})
	return out, nil
}

// Install installs a build from source: a local folder, a local .zip,
// .tar.gz or .tgz file, or an https URL to such an archive. wantSHA256 is
// required for URLs and optional for local archives.
func (m *Manager) Install(ctx context.Context, source, wantSHA256 string) (Build, error) {
	source = strings.TrimSpace(source)
	wantSHA256 = strings.ToLower(strings.TrimSpace(wantSHA256))
	if source == "" {
		return Build{}, fault.Invalidf("source is required: a folder, a .zip/.tar.gz/.tgz file or an https URL")
	}
	if wantSHA256 != "" && !ValidSHA256(wantSHA256) {
		return Build{}, fault.Invalidf("sha256 must be 64 hex characters")
	}
	work := tempDir + "/" + tempPrefix + randomName()
	tree := work + "/tree"
	defer func() {
		if err := m.st.RemoveAll(work); err != nil {
			m.log.Warn("cannot remove install work folder", "path", work, "err", err)
		}
	}()
	if err := m.st.MkdirAll(tree, 0o700); err != nil {
		return Build{}, err
	}
	w := newWriter(m.st, tree, m.opts.Limits)
	lower := strings.ToLower(source)
	switch {
	case strings.HasPrefix(lower, "http://"):
		return Build{}, fault.Invalidf("plain http downloads are refused; use an https URL with a sha256")
	case strings.HasPrefix(lower, "https://"):
		if wantSHA256 == "" {
			return Build{}, fault.Invalidf("a sha256 is required to install from a URL")
		}
		u, err := url.Parse(source)
		if err != nil || u.Host == "" || u.User != nil {
			return Build{}, fault.Invalidf("invalid URL (user names and passwords in URLs are not accepted)")
		}
		kind := archiveKind(path.Base(u.Path))
		if kind == "" {
			return Build{}, fault.Invalidf("the URL path must end in .zip, .tar.gz or .tgz")
		}
		archive := work + "/download." + kind
		if err := m.download(ctx, u, archive, wantSHA256); err != nil {
			return Build{}, err
		}
		abs, err := m.st.Abs(archive)
		if err != nil {
			return Build{}, err
		}
		if err := unpack(kind, abs, w); err != nil {
			return Build{}, err
		}
	default:
		abs, err := filepath.Abs(source)
		if err != nil {
			return Build{}, fault.Invalidf("invalid source path %q", source)
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return Build{}, fault.Invalidf("source %s: %v", abs, unwrapPathError(err))
		}
		if fi.IsDir() {
			if wantSHA256 != "" {
				return Build{}, fault.Invalidf("sha256 applies to archives and downloads, not folders")
			}
			if err := copyFolder(abs, w); err != nil {
				return Build{}, err
			}
			break
		}
		kind := archiveKind(abs)
		if kind == "" || !fi.Mode().IsRegular() {
			return Build{}, fault.Invalidf("source must be a folder, a .zip, .tar.gz or .tgz file, or an https URL")
		}
		if fi.Size() > m.opts.Limits.MaxBytes {
			return Build{}, fault.New(fault.Invalid, "too_large", "the archive is larger than %d bytes", m.opts.Limits.MaxBytes)
		}
		if wantSHA256 != "" {
			sum, err := fileSHA256(abs)
			if err != nil {
				return Build{}, err
			}
			if sum != wantSHA256 {
				return Build{}, fault.New(fault.Invalid, "hash_mismatch", "the archive's sha256 is %s, not %s", sum, wantSHA256)
			}
		}
		if err := unpack(kind, abs, w); err != nil {
			return Build{}, err
		}
	}
	return m.finish(tree)
}

func unwrapPathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func unpack(kind, abs string, w *writer) error {
	if kind == kindZip {
		return unpackZip(abs, w)
	}
	return unpackTarGz(abs, w)
}

// download fetches u into rel, checking size and sha256.
func (m *Manager) download(ctx context.Context, u *url.URL, rel, wantSHA256 string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fault.Invalidf("invalid URL: %v", err)
	}
	resp, err := m.opts.HTTPClient.Do(req)
	if err != nil {
		return fault.Wrap(fault.Failed, "download_failed", err, "download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fault.New(fault.Failed, "download_failed", "download failed: HTTP %s", resp.Status)
	}
	if resp.ContentLength > m.opts.Limits.MaxBytes {
		return fault.New(fault.Invalid, "too_large", "the download is larger than %d bytes", m.opts.Limits.MaxBytes)
	}
	f, err := m.st.Create(rel, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, m.opts.Limits.MaxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fault.Wrap(fault.Failed, "download_failed", err, "download failed")
	}
	if n > m.opts.Limits.MaxBytes {
		return fault.New(fault.Invalid, "too_large", "the download is larger than %d bytes", m.opts.Limits.MaxBytes)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != wantSHA256 {
		return fault.New(fault.Invalid, "hash_mismatch", "the download's sha256 is %s, not %s", sum, wantSHA256)
	}
	return nil
}

// finish checks the unpacked tree and moves it into builds/<version>.
func (m *Manager) finish(tree string) (Build, error) {
	top := tree
	if !m.st.Exists(tree + "/" + ManifestName) {
		// Accept archives that wrap the build in one top folder.
		entries, err := m.st.ReadDir(tree)
		if err != nil {
			return Build{}, err
		}
		if len(entries) == 1 && entries[0].IsDir() && m.st.Exists(tree+"/"+entries[0].Name()+"/"+ManifestName) {
			top = tree + "/" + entries[0].Name()
		} else {
			return Build{}, fault.New(fault.Invalid, "bad_manifest", "no %s at the top of the build", ManifestName)
		}
	}
	b, err := m.st.ReadFile(top + "/" + ManifestName)
	if err != nil {
		return Build{}, err
	}
	man, err := ParseManifest(b)
	if err != nil {
		return Build{}, err
	}
	if err := man.Check(m.opts.GOOS); err != nil {
		return Build{}, err
	}
	files, size, err := m.verify(top, man)
	if err != nil {
		return Build{}, err
	}
	if m.opts.GOOS != "windows" {
		if err := m.st.Chmod(top+"/"+man.Executables[m.opts.GOOS], 0o500); err != nil {
			return Build{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st.Exists(buildDir(man.Version)) {
		return Build{}, fault.New(fault.Conflict, "build_exists", "build %s is already installed", man.Version)
	}
	if err := m.st.MkdirAll(Dir, 0o700); err != nil {
		return Build{}, err
	}
	if err := m.st.Rename(top, buildDir(man.Version)); err != nil {
		return Build{}, err
	}
	in := info{Installed: time.Now().UTC(), Files: files + 1, SizeBytes: size + int64(len(b))}
	if err := m.st.WriteJSON(infoDir+"/"+man.Version+".json", in, 0o600); err != nil {
		m.log.Warn("cannot write build info", "version", man.Version, "err", err)
	}
	m.log.Info("build installed", "version", man.Version, "channel", man.Channel, "files", in.Files)
	m.publish("installed", man.Version)
	return m.Get(man.Version)
}

// verify checks that the folder holds exactly the files the manifest lists
// (plus the manifest) with matching hashes. It returns the file count and
// total size of the listed files.
func (m *Manager) verify(top string, man *Manifest) (files int, size int64, err error) {
	seen := make(map[string]bool, len(man.Files))
	var walk func(rel string) error
	walk = func(rel string) error {
		dir := top
		if rel != "" {
			dir = top + "/" + rel
		}
		entries, err := m.st.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if rel != "" {
				name = rel + "/" + name
			}
			switch {
			case e.IsDir():
				if err := walk(name); err != nil {
					return err
				}
				continue
			case !e.Type().IsRegular():
				return unsafeArchive("%q is not a regular file", name)
			case name == ManifestName:
				continue
			}
			want, ok := man.Files[name]
			if !ok {
				return fault.New(fault.Invalid, "bad_manifest", "%s is not listed in the manifest", name)
			}
			sum, n, err := m.hashFile(top + "/" + name)
			if err != nil {
				return err
			}
			if sum != want {
				return fault.New(fault.Invalid, "hash_mismatch", "%s has sha256 %s, the manifest says %s", name, sum, want)
			}
			seen[name] = true
			files++
			size += n
		}
		return nil
	}
	if err := walk(""); err != nil {
		return 0, 0, err
	}
	for rel := range man.Files {
		if !seen[rel] {
			return 0, 0, fault.New(fault.Invalid, "bad_manifest", "%s is listed in the manifest but missing", rel)
		}
	}
	return files, size, nil
}

func (m *Manager) hashFile(rel string) (string, int64, error) {
	f, err := m.st.Open(rel)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Remove deletes an installed build. The caller makes sure no instance
// uses it.
func (m *Manager) Remove(version string) error {
	if _, err := m.Get(version); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.st.RemoveAll(buildDir(version)); err != nil {
		return fmt.Errorf("remove build %s: %w", version, err)
	}
	m.st.Remove(infoDir + "/" + version + ".json")
	m.log.Info("build removed", "version", version)
	m.publish("removed", version)
	return nil
}

// Prune removes every installed build for which keep returns false and
// returns the removed versions.
func (m *Manager) Prune(keep func(version string) bool) ([]string, error) {
	list, err := m.List()
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, b := range list {
		if keep(b.Version) {
			continue
		}
		if err := m.Remove(b.Version); err != nil {
			return removed, err
		}
		removed = append(removed, b.Version)
	}
	return removed, nil
}

func (m *Manager) publish(action, version string) {
	if m.opts.Bus != nil {
		m.opts.Bus.Publish(events.TypeBuild, "", Event{Action: action, Version: version})
	}
}

// RedactSource returns source with any URL query, fragment and user
// information removed, for logs and the audit log.
func RedactSource(source string) string {
	u, err := url.Parse(strings.TrimSpace(source))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return source
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
