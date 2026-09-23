// Package updater is the Sparkle replacement: it checks a GitHub Releases
// feed (latest.json), verifies downloads (size + SHA-256 + ed25519 over the
// raw tarball), installs the new version, and restarts the daemon. The state
// machine mirrors the "update" object in docs/control-api.md.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

// AssetPlatform is the asset platform suffix for this machine.
func AssetPlatform() string {
	switch runtime.GOARCH {
	case "amd64":
		return "linux-amd64"
	case "arm64":
		return "linux-arm64"
	default:
		return "linux-" + runtime.GOARCH
	}
}

// DefaultFeedURL is the pinned location of latest.json on GitHub releases
// (Sparkle's SUFeedURL equivalent).
func DefaultFeedURL() string {
	return buildinfo.RepoURL + "/releases/latest/download/latest.json"
}

// FeedURL returns the feed to check, honoring DROIDPROXY_UPDATE_FEED
// (used by tests and staging channels).
func FeedURL() string {
	if v := os.Getenv("DROIDPROXY_UPDATE_FEED"); v != "" {
		return v
	}
	return DefaultFeedURL()
}

// SigVerifier checks an ed25519 signature over message. Injectable so tests
// can use their own keys.
type SigVerifier func(pubKey []byte, message, sig []byte) error

// Options configure an Updater. Zero values select defaults.
type Options struct {
	// FeedURL overrides FeedURL() (which already honors the env override).
	FeedURL string
	// HTTPClient fetches the feed and tarballs. Nil uses a sane default.
	HTTPClient *http.Client
	// Verify overrides ed25519 verification.
	Verify SigVerifier
	// Now overrides the clock.
	Now func() time.Time
	// OnStateChanged is called whenever the update state changed (the daemon
	// uses it to push a fresh state snapshot to control API clients).
	OnStateChanged func()
	// PostMessage reports flow results as control API "message" events.
	PostMessage func(title, body, level string)
	// OnUpdateAvailable is called when a manual or scheduled check finds an
	// update (the daemon notifies the user with an "Install" action). Not
	// called for automatic installs.
	OnUpdateAvailable func()
}

// Updater runs the update state machine.
type Updater struct {
	opts Options
	http *http.Client

	mu            sync.Mutex
	state         string
	latestVersion string
	notes         string
	releaseURL    string
	lastError     string
	lastChecked   time.Time
	progress      float64
	feed          *Feed // the feed that carried latestVersion
	installing    bool  // guards against concurrent installs
}

// New returns an Updater.
func New(opts Options) *Updater {
	if opts.FeedURL == "" {
		opts.FeedURL = FeedURL()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Verify == nil {
		opts.Verify = defaultVerifier
	}
	if opts.PostMessage == nil {
		opts.PostMessage = func(string, string, string) {}
	}
	if opts.OnStateChanged == nil {
		opts.OnStateChanged = func() {}
	}
	return &Updater{opts: opts, http: opts.HTTPClient, state: control.UpdateIdle}
}

// Snapshot returns the update section for the state snapshot.
func (u *Updater) Snapshot() control.UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return control.UpdateState{
		State:          u.state,
		CurrentVersion: buildinfo.Version,
		LatestVersion:  u.latestVersion,
		Notes:          u.notes,
		ReleaseURL:     u.releaseURL,
		Error:          u.lastError,
		LastChecked:    formatTime(u.lastChecked),
		Progress:       u.progress,
	}
}

func (u *Updater) setStateLocked(state string) {
	u.state = state
	u.opts.OnStateChanged()
}

func (u *Updater) resetProgressLocked() { u.progress = 0 }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Check checks the feed now. A dev build can check but never auto-installs.
func (u *Updater) Check(ctx context.Context) error {
	u.mu.Lock()
	if u.state == control.UpdateDownloading || u.state == control.UpdateInstalling || u.installing {
		u.mu.Unlock()
		return nil // a check in the middle of an install would only confuse things
	}
	u.lastError = ""
	u.setStateLocked(control.UpdateChecking)
	u.mu.Unlock()

	feed, err := u.fetchFeed(ctx)
	if err != nil {
		u.mu.Lock()
		u.lastError = err.Error()
		u.setStateLocked(control.UpdateError)
		u.mu.Unlock()
		logx.Logf("[Updater] Check failed: %v", err)
		return err
	}

	u.mu.Lock()
	u.feed = feed
	u.latestVersion = feed.Version
	u.notes = feed.Notes
	u.releaseURL = feed.ReleaseURL
	u.lastChecked = u.opts.Now()

	_ = prefs.Shared().Set(prefs.KeyLastUpdateCheck, u.lastChecked.Unix())

	switch {
	case feed.Version == "":
		u.lastError = "update feed is missing a version"
		u.setStateLocked(control.UpdateError)
	case CompareVersions(feed.Version, buildinfo.Version) <= 0:
		u.resetProgressLocked()
		u.setStateLocked(control.UpdateUpToDate)
	case buildinfo.IsDev():
		// Dev builds report availability but never touch the installation.
		u.resetProgressLocked()
		u.setStateLocked(control.UpdateAvailable)
		onAvailable := u.opts.OnUpdateAvailable
		u.mu.Unlock()
		if onAvailable != nil {
			onAvailable()
		}
		return nil
	default:
		u.resetProgressLocked()
		u.setStateLocked(control.UpdateAvailable)
		if prefs.AutoInstallUpdates() {
			u.mu.Unlock()
			go func() {
				if err := u.Install(context.WithoutCancel(ctx)); err != nil {
					logx.Logf("[Updater] Auto-install failed: %v", err)
				}
			}()
			return nil
		}
		onAvailable := u.opts.OnUpdateAvailable
		u.mu.Unlock()
		if onAvailable != nil {
			onAvailable()
		}
		return nil
	}
	u.mu.Unlock()
	return nil
}

// Install downloads, verifies, extracts, runs the new version's installer,
// and restarts. It is a no-op when nothing is available or an install is
// already running.
func (u *Updater) Install(ctx context.Context) error {
	u.mu.Lock()
	if u.installing || u.state != control.UpdateAvailable {
		state := u.state
		u.mu.Unlock()
		if state != control.UpdateAvailable {
			return fmt.Errorf("updater: nothing to install (state %q)", state)
		}
		return errors.New("updater: install already running")
	}
	feed := u.feed
	if feed == nil || feed.Version == "" {
		u.mu.Unlock()
		return errors.New("updater: no update found; check first")
	}
	if buildinfo.IsDev() {
		u.mu.Unlock()
		return errors.New("updater: dev builds cannot self-install")
	}
	if CompareVersions(feed.Version, buildinfo.Version) <= 0 {
		u.mu.Unlock()
		return fmt.Errorf("updater: refusing to downgrade to %s", feed.Version)
	}
	u.installing = true
	version := feed.Version
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		u.installing = false
		u.mu.Unlock()
	}()

	tarball, err := u.download(ctx, feed)
	if err != nil {
		u.fail(err)
		return err
	}

	u.mu.Lock()
	u.setStateLocked(control.UpdateInstalling)
	u.mu.Unlock()

	dir, err := u.extract(tarball, version)
	if err != nil {
		u.fail(err)
		return err
	}

	if err := u.runNewInstaller(dir); err != nil {
		u.fail(err)
		return err
	}

	if err := pruneOldVersions(version); err != nil {
		// Pruning is housekeeping; a failure here must not block the update.
		logx.Logf("[Updater] Prune: %v", err)
	}

	u.opts.PostMessage("Software Update", "DroidProxy "+version+" installed. Restarting…", control.LevelInfo)
	u.restart()
	return nil
}

func (u *Updater) fail(err error) {
	logx.Logf("[Updater] Install failed: %v", err)
	u.mu.Lock()
	u.lastError = err.Error()
	u.setStateLocked(control.UpdateError)
	u.mu.Unlock()
	u.opts.PostMessage("Software Update", err.Error(), control.LevelError)
}

// fetchFeed gets and parses latest.json.
func (u *Updater) fetchFeed(ctx context.Context) (*Feed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.opts.FeedURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach update feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update feed returned HTTP %d", resp.StatusCode)
	}
	var feed Feed
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&feed); err != nil {
		return nil, fmt.Errorf("update feed is not valid JSON: %w", err)
	}
	return &feed, nil
}

// download fetches the platform tarball into paths.InstallRoot()/downloads,
// verifying size, sha256, and signature on the fly, and returns its path.
func (u *Updater) download(ctx context.Context, feed *Feed) (string, error) {
	platform := AssetPlatform()
	asset, ok := feed.Assets[platform]
	if !ok || asset.URL == "" {
		return "", fmt.Errorf("update has no %s asset", platform)
	}

	dlDir := filepath.Join(paths.InstallRoot(), "downloads")
	if err := paths.EnsureDir(dlDir, 0o700); err != nil {
		return "", err
	}
	finalPath := filepath.Join(dlDir, filepath.Base(asset.URL))
	if !strings.HasSuffix(finalPath, ".tar.gz") {
		return "", fmt.Errorf("asset URL does not name a .tar.gz: %s", asset.URL)
	}
	tmpPath := finalPath + ".part"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpPath) // no-op after a successful rename

	h := newHasher()
	total := asset.Size
	var written int64
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return "", werr
			}
			h.Write(buf[:n])
			written += int64(n)
			u.mu.Lock()
			if total > 0 {
				u.progress = float64(written) / float64(total)
				if u.progress > 1 {
					u.progress = 1
				}
			}
			u.opts.OnStateChanged()
			u.mu.Unlock()
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			out.Close()
			return "", rerr
		}
	}
	if err := out.Close(); err != nil {
		return "", err
	}

	// Size check first: it is the cheapest way to reject a truncated body.
	if total > 0 && written != total {
		return "", fmt.Errorf("downloaded %d bytes, expected %d", written, total)
	}
	sum := h.Sum()
	if asset.SHA256 != "" && sum != asset.SHA256 {
		return "", fmt.Errorf("SHA-256 mismatch: got %s, want %s", sum, asset.SHA256)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", err
	}
	// The signature is over the raw tarball bytes, so verify against the file
	// on disk after the atomic rename (the hash above only covered SHA-256).
	fileBytes, err := os.ReadFile(finalPath)
	if err != nil {
		return "", err
	}
	if err := u.verifySignature(asset.Signature, fileBytes); err != nil {
		return "", err
	}
	return finalPath, nil
}

// verifySignature checks the base64 ed25519 signature over the raw tarball
// bytes against the embedded public key.
func (u *Updater) verifySignature(sigB64 string, digest []byte) error {
	if sigB64 == "" {
		return errors.New("update is not signed")
	}
	pub, err := base64Decode(PublicKey)
	if err != nil || len(pub) != 32 {
		return errors.New("embedded public key is invalid; refuse to verify updates")
	}
	sig, err := base64Decode(sigB64)
	if err != nil {
		return errors.New("update signature is not valid base64")
	}
	if err := u.opts.Verify(pub, digest, sig); err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	return nil
}

// extract unpacks the tarball into paths.VersionsDir()/version. Extraction
// goes through a temp dir under InstallRoot so a partial download never looks
// like an installed version.
func (u *Updater) extract(tarball, version string) (string, error) {
	dest := filepath.Join(paths.VersionsDir(), version)
	if _, err := os.Stat(dest); err == nil {
		// Left over from an interrupted install; safe to reuse only if it
		// looks complete, so just remove and re-extract.
		if err := os.RemoveAll(dest); err != nil {
			return "", err
		}
	}
	if err := ExtractTarGz(tarball, filepath.Dir(dest), version); err != nil {
		return "", err
	}
	bin := filepath.Join(dest, "bin", "droidproxy")
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return "", fmt.Errorf("extracted update has no bin/droidproxy")
	}
	return dest, nil
}

// runNewInstaller hands over to the new version so IT lays down its own
// plugin, unit, desktop entry, and icons.
// runNewInstallerHook, when set (tests), replaces the real invocation.
var runNewInstallerHook func(bin string, args ...string) error

func (u *Updater) runNewInstaller(dir string) error {
	bin := filepath.Join(dir, "bin", "droidproxy")
	if runNewInstallerHook != nil {
		return runNewInstallerHook(bin, "install", "--from", dir, "--update")
	}
	cmd := newCmd(bin, "install", "--from", dir, "--update")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("new version install failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	logx.Logf("[Updater] New version installer output: %s", strings.TrimSpace(string(out)))
	return nil
}

// restart moves the daemon to the new version: systemctl when running as a
// user unit, otherwise re-exec into the freshly linked binary.
func (u *Updater) restart() {
	if underSystemd() {
		logx.Logf("[Updater] Restarting via systemd")
		if err := restartService(); err != nil {
			logx.Logf("[Updater] systemd restart failed: %v", err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		logx.Logf("[Updater] Cannot re-exec: %v", err)
		return
	}
	args := []string{exe, "serve"}
	logx.Logf("[Updater] Re-exec into %s", exe)
	if err := reExec(exe, args, os.Environ()); err != nil {
		logx.Logf("[Updater] Re-exec failed: %v", err)
	}
}

// StartScheduled runs the Sparkle parity schedule: one check ~60s after
// start when the last check is older than 24h, then re-evaluation hourly.
// It returns immediately; the loop ends when ctx is done.
func (u *Updater) StartScheduled(ctx context.Context) {
	go func() {
		timer := time.NewTimer(60 * time.Second)
		defer timer.Stop()
		hourly := time.NewTicker(time.Hour)
		defer hourly.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if prefs.AutoCheckUpdates() && timeSinceLastCheck(u.opts.Now) > 24*time.Hour {
					if err := u.Check(ctx); err != nil {
						logx.Logf("[Updater] Scheduled check failed: %v", err)
					}
				}
			case <-hourly.C:
				if prefs.AutoCheckUpdates() && timeSinceLastCheck(u.opts.Now) > time.Hour {
					if err := u.Check(ctx); err != nil {
						logx.Logf("[Updater] Scheduled check failed: %v", err)
					}
				}
			}
		}
	}()
}

func timeSinceLastCheck(now func() time.Time) time.Duration {
	var last int64
	if prefs.Shared().Get(prefs.KeyLastUpdateCheck, &last) {
		return now().Sub(time.Unix(last, 0))
	}
	return 1 << 62 // effectively forever: never checked
}
