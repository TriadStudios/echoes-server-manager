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
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
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
	GOOS string // default runtime.GOOS
	// SteamAppID, when set, is written to gameserver.SteamAppIDFile next to
	// the server binary of every build installed from now on, before the
	// build is made read-only (see gameserver.SteamAppIDFile).
	SteamAppID string
	Limits     Limits       // zero fields take the defaults
	ImportDir  string       // absolute path of the folder local sources must be in
	HTTPClient *http.Client // for https sources; default: no overall timeout, https-only redirects
	Bus        *events.Bus  // optional: build events are published here
	Logger     *slog.Logger
}

// Build describes an installed build.
type Build struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	Channel    string `json:"channel"`
	DefaultMap string `json:"default_map"` // empty: instances must name their map
	// Executable is the server binary for this OS: a slash path inside the
	// build.
	Executable string    `json:"executable"`
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
	out := Build{Version: version, Commit: man.Commit, Channel: man.Channel, DefaultMap: man.DefaultMap,
		Executable: man.Executables[m.opts.GOOS], Files: len(man.Files) + 1}
	var in info
	if err := m.st.ReadJSON(infoDir+"/"+version+".json", &in); err == nil {
		out.Installed, out.Files, out.SizeBytes = in.Installed, in.Files, in.SizeBytes
	} else if fi, err := m.st.Stat(buildDir(version) + "/" + ManifestName); err == nil {
		out.Installed = fi.ModTime().UTC()
	}
	return out, nil
}

// Path returns the absolute path of an installed build's top folder: the
// working folder of its servers.
func (m *Manager) Path(version string) (string, error) {
	if _, err := m.Get(version); err != nil {
		return "", err
	}
	return m.st.Abs(buildDir(version))
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

// source is an install request read: what to install and from where.
type source struct {
	text   string   // the source as given, trimmed
	sha256 string   // lower case, or ""
	remote *url.URL // set for an https source
	kind   string   // the archive kind of a remote source
}

// parseSource reads and checks what a request to install a build asks for,
// without touching any file: the source is given, the sha256 is one, a URL is
// https with a sha256 and names an archive. A local source is looked at by
// openLocal.
func parseSource(text, wantSHA256 string) (source, error) {
	src := source{text: strings.TrimSpace(text), sha256: strings.ToLower(strings.TrimSpace(wantSHA256))}
	if src.text == "" {
		return source{}, fault.Invalidf("source is required: a folder, a .zip/.tar.gz/.tgz file or an https URL")
	}
	if src.sha256 != "" && !ValidSHA256(src.sha256) {
		return source{}, fault.Invalidf("sha256 must be 64 hex characters")
	}
	lower := strings.ToLower(src.text)
	switch {
	case strings.HasPrefix(lower, "http://"):
		return source{}, fault.Invalidf("plain http downloads are refused; use an https URL with a sha256")
	case strings.HasPrefix(lower, "https://"):
		if src.sha256 == "" {
			return source{}, fault.Invalidf("a sha256 is required to install from a URL")
		}
		u, err := url.Parse(src.text)
		if err != nil || u.Host == "" || u.User != nil {
			return source{}, fault.Invalidf("invalid URL (user names and passwords in URLs are not accepted)")
		}
		src.kind = archiveKind(path.Base(u.Path))
		if src.kind == "" {
			return source{}, fault.Invalidf("the URL path must end in .zip, .tar.gz or .tgz")
		}
		src.remote = u
	}
	return src, nil
}

// CheckInstall says whether Install would take its request, as far as the
// request and the import folder tell, without installing anything: what
// Install refuses before it reads or downloads a byte. It is what a call that
// starts the install as a job answers at once.
func (m *Manager) CheckInstall(source, wantSHA256 string) error {
	src, err := parseSource(source, wantSHA256)
	if err != nil || src.remote != nil {
		return err
	}
	ls, err := m.openLocal(src.text, src.sha256)
	if err != nil {
		return err
	}
	ls.Close()
	return nil
}

// Install installs a build from source: a local folder, a local .zip,
// .tar.gz or .tgz file, or an https URL to such an archive. wantSHA256 is
// required for URLs and optional for local archives. In a job its steps are
// download (a URL only; with the bytes so far), unpack (copy a folder, or
// check an archive's sha256 and unpack it), verify and place. The job can
// be cancelled until place begins.
func (m *Manager) Install(ctx context.Context, source, wantSHA256 string) (Build, error) {
	src, err := parseSource(source, wantSHA256)
	if err != nil {
		return Build{}, err
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
	w.stop = func() error { return jobs.Interrupted(ctx) }
	if src.remote != nil {
		jobs.Step(ctx, "download")
		archive := work + "/download." + src.kind
		if err := m.download(ctx, src.remote, archive, src.sha256); err != nil {
			return Build{}, err
		}
		jobs.Step(ctx, "unpack")
		f, err := m.st.Open(archive)
		if err != nil {
			return Build{}, err
		}
		defer f.Close()
		if err := unpack(src.kind, f, w); err != nil {
			return Build{}, err
		}
	} else {
		jobs.Step(ctx, "unpack")
		if err := m.installLocal(src.text, src.sha256, w); err != nil {
			return Build{}, err
		}
	}
	return m.finish(ctx, tree)
}

// notAllowed refuses a local source outside the import folder. It says
// nothing about the source itself (whether it exists, what it is, who may
// read it), only where sources may be.
func (m *Manager) notAllowed() error {
	if m.opts.ImportDir == "" {
		return fault.New(fault.Invalid, "source_not_allowed", "local sources are off: give an https URL")
	}
	return fault.New(fault.Invalid, "source_not_allowed",
		"a local source must be a readable folder or archive inside the import folder %s (or give an https URL)", m.opts.ImportDir)
}

// localSource is a local source that was found and looked at.
type localSource struct {
	root *os.Root
	f    *os.File
	rel  string
	dir  bool
	kind string // the archive kind, for a file
}

// Close closes what the source opened.
func (l *localSource) Close() {
	l.f.Close()
	l.root.Close()
}

// localPath is the path that a local source names: a relative one is taken
// from the import folder dir, any other is the path it names, which the
// caller then finds inside dir or refuses. "Any other" is more than
// filepath.IsAbs says on Windows, where "/etc" and "\etc" are no absolute
// paths (they name the root of the current drive) and "C:etc" names a place
// on a drive, none of them a name below the import folder: a path that
// starts with a separator or has a volume name is made absolute, as the
// system does it. On Linux such a path is absolute already. A UNC path
// (\\server\share\x) is absolute and has a volume name.
func localPath(dir, source string) (string, error) {
	if filepath.IsAbs(source) || filepath.VolumeName(source) != "" || (source != "" && os.IsPathSeparator(source[0])) {
		return filepath.Abs(source)
	}
	return filepath.Join(dir, source), nil
}

// openLocal finds a local source and checks what can be checked of it
// without reading it: it must lie inside the import folder (a relative path
// is taken from there; see localPath for what is not one), and it is opened
// through an os.Root on that folder, so neither ".." nor a link can lead out
// of it. What is outside the folder is refused as such, with no word of what
// is there. It must be a folder, or a regular archive that is not above the
// size limit.
func (m *Manager) openLocal(source, wantSHA256 string) (*localSource, error) {
	dir := m.opts.ImportDir
	if dir == "" {
		return nil, m.notAllowed()
	}
	abs, err := localPath(dir, source)
	if err != nil {
		return nil, m.notAllowed()
	}
	rel, err := filepath.Rel(dir, filepath.Clean(abs))
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return nil, m.notAllowed()
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open the import folder: %w", err)
	}
	f, err := root.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		root.Close()
		return nil, fault.Invalidf("there is no %s in the import folder %s", filepath.ToSlash(rel), dir)
	}
	if err != nil {
		root.Close()
		return nil, m.notAllowed()
	}
	ls := &localSource{root: root, f: f, rel: rel}
	fi, err := f.Stat()
	if err != nil {
		ls.Close()
		return nil, m.notAllowed()
	}
	if fi.IsDir() {
		if wantSHA256 != "" {
			ls.Close()
			return nil, fault.Invalidf("sha256 applies to archives and downloads, not folders")
		}
		ls.dir = true
		return ls, nil
	}
	ls.kind = archiveKind(rel)
	if ls.kind == "" || !fi.Mode().IsRegular() {
		ls.Close()
		return nil, fault.Invalidf("source must be a folder, a .zip, .tar.gz or .tgz file, or an https URL")
	}
	if fi.Size() > m.opts.Limits.MaxBytes {
		ls.Close()
		return nil, fault.New(fault.Invalid, "too_large", "the archive is larger than %d bytes", m.opts.Limits.MaxBytes)
	}
	return ls, nil
}

// installLocal copies or unpacks a local source (see openLocal) into w.
func (m *Manager) installLocal(source, wantSHA256 string, w *writer) error {
	ls, err := m.openLocal(source, wantSHA256)
	if err != nil {
		return err
	}
	defer ls.Close()
	if ls.dir {
		sub, err := ls.root.OpenRoot(ls.rel)
		if err != nil {
			return m.notAllowed()
		}
		defer sub.Close()
		return copyFolder(sub, w)
	}
	if wantSHA256 != "" {
		h := sha256.New()
		if _, err := io.Copy(h, ls.f); err != nil {
			return err
		}
		if sum := hex.EncodeToString(h.Sum(nil)); sum != wantSHA256 {
			return fault.New(fault.Invalid, "hash_mismatch", "the archive's sha256 is %s, not %s", sum, wantSHA256)
		}
		if _, err := ls.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	return unpack(ls.kind, ls.f, w)
}

// unpack extracts the archive in f into w.
func unpack(kind string, f *os.File, w *writer) error {
	if kind == kindZip {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		return unpackZip(f, fi.Size(), w)
	}
	return unpackTarGz(f, w)
}

// download fetches u into rel, checking size and sha256.
func (m *Manager) download(ctx context.Context, u *url.URL, rel, wantSHA256 string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fault.Invalidf("invalid URL: %v", err)
	}
	// An answer names no more than the class of the server's status; the
	// details (addresses, messages of the server) go to the service log
	// only, so a download cannot serve to probe other machines.
	resp, err := m.opts.HTTPClient.Do(req)
	if err != nil {
		m.log.Warn("build download failed", "url", RedactSource(u.String()), "err", err)
		return fault.New(fault.Failed, "download_failed", "download failed: no answer from the server (details in the service log)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		m.log.Warn("build download failed", "url", RedactSource(u.String()), "status", resp.Status)
		return fault.New(fault.Failed, "download_failed", "download failed: the server answered with a %dxx status", resp.StatusCode/100)
	}
	if resp.ContentLength > m.opts.Limits.MaxBytes {
		return fault.New(fault.Invalid, "too_large", "the download is larger than %d bytes", m.opts.Limits.MaxBytes)
	}
	f, err := m.st.Create(rel, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(jobs.Writer(ctx, max(resp.ContentLength, 0), io.MultiWriter(f, h)), io.LimitReader(resp.Body, m.opts.Limits.MaxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		m.log.Warn("build download failed", "url", RedactSource(u.String()), "err", err)
		return fault.New(fault.Failed, "download_failed", "download failed: the transfer broke off (details in the service log)")
	}
	if n > m.opts.Limits.MaxBytes {
		return fault.New(fault.Invalid, "too_large", "the download is larger than %d bytes", m.opts.Limits.MaxBytes)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != wantSHA256 {
		return fault.New(fault.Invalid, "hash_mismatch", "the download's sha256 is %s, not %s", sum, wantSHA256)
	}
	return nil
}

// finish checks the unpacked tree and moves it into builds/<version>. The
// job of ctx, if any, is in its verify step until the tree is checked, and
// in its place step from then on: it cannot be cancelled any more.
func (m *Manager) finish(ctx context.Context, tree string) (Build, error) {
	jobs.Step(ctx, "verify")
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
	files, size, err := m.verify(ctx, top, man)
	if err != nil {
		return Build{}, err
	}
	if err := jobs.Commit(ctx, "place"); err != nil {
		return Build{}, err
	}
	placed, err := m.placeSteamAppID(top, man)
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
	if placed {
		// The app id file ESM added counts like any file of the build.
		in.Files++
		in.SizeBytes += int64(len(m.opts.SteamAppID))
	}
	if err := m.st.WriteJSON(infoDir+"/"+man.Version+".json", in, 0o600); err != nil {
		m.log.Warn("cannot write build info", "version", man.Version, "err", err)
	}
	m.log.Info("build installed", "version", man.Version, "channel", man.Channel, "files", in.Files)
	m.publish("installed", man.Version)
	return m.Get(man.Version)
}

// placeSteamAppID writes the configured Steam app id next to the server
// binary for this OS, as a read-only file like every file of the build,
// and reports whether it did. A build that brings the file along keeps its
// own. Nothing is written when no app id is configured (doctor warns
// about that).
func (m *Manager) placeSteamAppID(top string, man *Manifest) (bool, error) {
	if m.opts.SteamAppID == "" {
		return false, nil
	}
	rel := path.Join(path.Dir(man.Executables[m.opts.GOOS]), gameserver.SteamAppIDFile)
	if m.st.Exists(top + "/" + rel) {
		m.log.Info("the build has its own Steam app id file; leaving it", "version", man.Version, "file", rel)
		return false, nil
	}
	f, err := m.st.Create(top+"/"+rel, 0o400)
	if err != nil {
		return false, fmt.Errorf("write %s: %w", rel, err)
	}
	_, err = f.WriteString(m.opts.SteamAppID)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, fmt.Errorf("write %s: %w", rel, err)
	}
	return true, nil
}

// verify checks that the folder holds exactly the files the manifest lists
// (plus the manifest) with matching hashes. It returns the file count and
// total size of the listed files.
func (m *Manager) verify(ctx context.Context, top string, man *Manifest) (files int, size int64, err error) {
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
			if err := jobs.Interrupted(ctx); err != nil {
				return err
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
