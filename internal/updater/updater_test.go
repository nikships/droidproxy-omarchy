package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
)

// testEnv is a fake release feed over a local HTTP server, with its own
// ed25519 keypair and a "current" installation on disk.
type testEnv struct {
	t        *testing.T
	srv      *httptest.Server
	priv     ed25519.PrivateKey
	feed     *Feed
	platform string
	// recorded side effects
	installerArgs []string
	restarts      int

	tarballBytes   []byte
	tarballVersion string
	failFeed       bool
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("DROIDPROXY_UPDATE_FEED", "")
	t.Setenv("DROIDPROXY_ROOT", filepath.Join(t.TempDir(), "resource-root")) // keep the real install out of reach

	// Tests run against a fixed release version so install flows behave like
	// production (dev builds refuse to self-install).
	oldVersion := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = oldVersion })

	e := &testEnv{t: t, platform: AssetPlatform()}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.priv = priv

	e.writeCurrentInstallation()

	mux := http.NewServeMux()
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) {
		if e.failFeed {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		feedJSON, _ := json.Marshal(e.feed)
		w.Write(feedJSON)
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	next := nextVersion(buildinfo.Version)
	e.tarballFor(next)
	e.serveTarball(next)

	// The "install" step runs the new binary's installer; intercept it.
	oldHook := runNewInstallerHook
	runNewInstallerHook = func(bin string, args ...string) error {
		e.installerArgs = append([]string{bin}, args...)
		// Simulate the installer's effect: flip the current symlink.
		version := filepath.Base(filepath.Dir(filepath.Dir(bin)))
		flipCurrent(t, version)
		return nil
	}
	t.Cleanup(func() { runNewInstallerHook = oldHook })

	// Simulate the systemd path so restartService is the exercised branch
	// (the non-systemd path re-execs, which a test binary must never do).
	underSystemd = func() bool { return true }
	restartService = func() error {
		e.restarts++
		return nil
	}
	reExec = func(string, []string, []string) error { return nil }
	t.Cleanup(func() {
		underSystemd = desktop.UnderSystemd
		restartService = desktop.Restart
		reExec = syscallExec
	})
	_ = pub
	return e
}

func (e *testEnv) tarballFor(version string) {
	e.buildAndSignTarball(version)
}

func (e *testEnv) buildAndSignTarball(version string) {
	e.tarballBytes = buildTarball(e.t, version, e.platform)
	e.tarballVersion = version
	e.signTarball()
}

func (e *testEnv) signTarball() {
	sum := sha256.Sum256(e.tarballBytes)
	e.feed = &Feed{
		Version:    e.tarballVersion,
		Tag:        "v" + e.tarballVersion,
		Notes:      "Bug fixes and improvements",
		ReleaseURL: "https://github.com/" + buildinfo.Repo + "/releases/tag/v" + e.tarballVersion,
		Assets: map[string]Asset{
			e.platform: {
				URL:       e.srv.URL + "/droidproxy-" + e.tarballVersion + "-" + e.platform + ".tar.gz",
				SHA256:    fmt.Sprintf("%x", sum),
				Size:      int64(len(e.tarballBytes)),
				Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(e.priv, e.tarballBytes)),
			},
		},
	}
}

func (e *testEnv) serveTarball(version string) {
	mux := e.srv.Config.Handler.(*http.ServeMux)
	mux.HandleFunc("/droidproxy-"+version+"-"+e.platform+".tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(e.tarballBytes)
	})
}

func (e *testEnv) writeCurrentInstallation() {
	t := e.t
	cur := currentVersionForTest(t)
	dir := filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/versions", cur)
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "droidproxy"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	flipCurrent(t, cur)
}

func flipCurrent(t *testing.T, version string) {
	t.Helper()
	root := filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, ".current.tmp")
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("versions", version), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
}

func currentVersionForTest(*testing.T) string { return buildinfo.Version }

func buildTarball(t *testing.T, version, platform string) []byte {
	t.Helper()
	top := "droidproxy-" + version + "-" + platform
	files := map[string][]byte{
		top + "/VERSION":                          []byte(version),
		top + "/bin/droidproxy":                   []byte("#!/bin/sh\nexit 0\n"),
		top + "/share/systemd/droidproxy.service": []byte("unit"),
		top + "/share/plugin/manifest.json":       []byte("{}"),
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(content)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// nextVersion returns a version strictly newer than v.
func nextVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	last := parts[len(parts)-1]
	var n int
	fmt.Sscanf(last, "%d", &n)
	parts[len(parts)-1] = fmt.Sprint(n + 1)
	return strings.Join(parts, ".")
}

func (e *testEnv) updater() *Updater {
	e.t.Helper()
	// The verifier ignores the embedded (placeholder) key and checks against
	// the test key instead; production passes ed25519 verification of the
	// same shape.
	return New(Options{
		FeedURL: e.srv.URL + "/latest.json",
		Verify: func(pubKey, message, sig []byte) error {
			if !ed25519.Verify(e.priv.Public().(ed25519.PublicKey), message, sig) {
				return fmt.Errorf("bad signature")
			}
			return nil
		},
		Now: time.Now,
	})
}

// ---- tests ------------------------------------------------------------------

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0", "1.0.0", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3-beta", "1.2.3", -1},
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"0.0.0-dev", "1.0.0", -1},
		{"", "1.0.0", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCheckFindsUpdate(t *testing.T) {
	e := newTestEnv(t)
	u := e.updater()

	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	snap := u.Snapshot()
	if snap.State != control.UpdateAvailable {
		t.Fatalf("state = %s, want available (err %q)", snap.State, snap.Error)
	}
	if snap.LatestVersion == currentVersionForTest(t) {
		t.Errorf("latestVersion = %q, want newer", snap.LatestVersion)
	}
	if snap.LastChecked == "" || snap.Notes == "" || snap.ReleaseURL == "" {
		t.Errorf("snapshot incomplete: %+v", snap)
	}
}

func TestCheckUpToDate(t *testing.T) {
	e := newTestEnv(t)
	cur := currentVersionForTest(t)
	e.buildAndSignTarball(cur) // feed now advertises the current version
	u := e.updater()

	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if st := u.Snapshot().State; st != control.UpdateUpToDate {
		t.Fatalf("state = %s, want upToDate", st)
	}
}

func TestCheckFeedFailureSetsErrorState(t *testing.T) {
	e := newTestEnv(t)
	e.failFeed = true
	u := e.updater()
	if err := u.Check(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	snap := u.Snapshot()
	if snap.State != control.UpdateError || snap.Error == "" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

func TestInstallFullFlow(t *testing.T) {
	e := newTestEnv(t)
	u := e.updater()

	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := u.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// The new version's installer ran with the right flags.
	if len(e.installerArgs) == 0 {
		t.Fatal("new version installer was not run")
	}
	joined := strings.Join(e.installerArgs, " ")
	if !strings.Contains(joined, "install") || !strings.Contains(joined, "--from") || !strings.Contains(joined, "--update") {
		t.Errorf("installer args wrong: %v", e.installerArgs)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/versions", e.feed.Version, "bin/droidproxy")); err != nil {
		t.Errorf("extracted version missing: %v", err)
	}
	if e.restarts != 1 {
		t.Errorf("restarts = %d, want 1", e.restarts)
	}
}

func TestInstallRefusesTamperedTarball(t *testing.T) {
	e := newTestEnv(t)
	u := e.updater()

	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}

	// Flip one byte of the served tarball, re-sign it with the right key,
	// but keep the SHA-256 from the original: it must fail on the hash.
	orig := e.tarballBytes
	tampered := append([]byte(nil), orig...)
	tampered[len(tampered)-1] ^= 0xff
	e.tarballBytes = tampered
	a := e.feed.Assets[e.platform]
	a.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(e.priv, tampered))
	e.feed.Assets[e.platform] = a
	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("re-Check: %v", err)
	}
	err := u.Install(context.Background())
	if err == nil {
		t.Fatal("expected failure")
	}
	if st := u.Snapshot().State; st != control.UpdateError {
		t.Fatalf("state = %s, want error", st)
	}
	// Nothing was installed.
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/versions", e.feed.Version)); err == nil {
		t.Error("tampered version was extracted")
	}
	if len(e.installerArgs) != 0 || e.restarts != 0 {
		t.Error("install flow ran past verification")
	}
}

func TestInstallRefusesBadSignature(t *testing.T) {
	e := newTestEnv(t)
	u := e.updater()

	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	// Correct sha256, wrong signature (signed by another key).
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	a := e.feed.Assets[e.platform]
	a.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(other, e.tarballBytes))
	e.feed.Assets[e.platform] = a
	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("re-Check: %v", err)
	}

	if err := u.Install(context.Background()); err == nil {
		t.Fatal("expected signature failure")
	}
	if !strings.Contains(u.Snapshot().Error, "signature") {
		t.Errorf("error = %q, want signature failure", u.Snapshot().Error)
	}
}

func TestInstallRefusesWrongSize(t *testing.T) {
	e := newTestEnv(t)
	u := e.updater()
	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	a := e.feed.Assets[e.platform]
	a.Size = 1 // served tarball is much larger
	e.feed.Assets[e.platform] = a
	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("re-Check: %v", err)
	}
	if err := u.Install(context.Background()); err == nil {
		t.Fatal("expected size failure")
	}
	if !strings.Contains(u.Snapshot().Error, "bytes") {
		t.Errorf("error = %q, want size mismatch", u.Snapshot().Error)
	}
}

func TestDevBuildCannotInstall(t *testing.T) {
	e := newTestEnv(t)
	// Make the build look like a dev build again (newTestEnv pinned a
	// release version).
	oldVersion := buildinfo.Version
	buildinfo.Version = "0.0.0-dev"
	t.Cleanup(func() { buildinfo.Version = oldVersion })

	u := e.updater()
	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if st := u.Snapshot().State; st != control.UpdateAvailable {
		t.Fatalf("dev builds should see availability, got %s", st)
	}
	if err := u.Install(context.Background()); err == nil {
		t.Fatal("dev install should be refused")
	}
	if len(e.installerArgs) != 0 || e.restarts != 0 {
		t.Error("dev install ran past the refusal")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	// A tarball whose entry escapes via ..
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "droidproxy-1.0.0-linux-amd64/../../evil", Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()

	bad := filepath.Join(dir, "bad.tar.gz")
	if err := os.WriteFile(bad, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(bad, dir, "1.0.0"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("traversal file was written")
	}

	// A symlinked entry is also rejected.
	buf.Reset()
	gz = gzip.NewWriter(&buf)
	tw = tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "droidproxy-1.0.0-linux-amd64/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()
	sym := filepath.Join(dir, "sym.tar.gz")
	if err := os.WriteFile(sym, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(sym, dir, "1.0.0"); err == nil {
		t.Fatal("expected symlink to be rejected")
	}
}

func TestExtractRejectsWrongTopDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "something-else/bin/x", Mode: 0o755, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	p := filepath.Join(dir, "x.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(p, dir, "1.0.0"); err == nil {
		t.Fatal("expected top-dir mismatch to be rejected")
	}
}

func TestPruneKeepsCurrentAndOnePrevious(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	dir := filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/versions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0"} {
		if err := os.MkdirAll(filepath.Join(dir, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneOldVersions("1.3.0"); err != nil {
		t.Fatalf("pruneOldVersions: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sortStrings(got)
	if len(got) != 2 || got[0] != "1.2.0" || got[1] != "1.3.0" {
		t.Errorf("versions after prune = %v, want [1.2.0 1.3.0]", got)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestFeedURLFromEnv(t *testing.T) {
	t.Setenv("DROIDPROXY_UPDATE_FEED", "https://example.test/feed.json")
	if got := FeedURL(); got != "https://example.test/feed.json" {
		t.Errorf("FeedURL = %q", got)
	}
	t.Setenv("DROIDPROXY_UPDATE_FEED", "")
	if got := FeedURL(); got != DefaultFeedURL() {
		t.Errorf("FeedURL = %q, want default", got)
	}
}

func TestSnapshotShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	u := New(Options{Now: func() time.Time { return time.Unix(1700000000, 0) }})
	snap := u.Snapshot()
	if snap.State != control.UpdateIdle || snap.Progress != 0 || snap.LastChecked != "" {
		t.Errorf("unexpected initial snapshot: %+v", snap)
	}
	if snap.CurrentVersion != buildinfo.Version {
		t.Errorf("currentVersion = %q", snap.CurrentVersion)
	}
}
