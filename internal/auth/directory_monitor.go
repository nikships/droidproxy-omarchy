package auth

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// DirectoryMonitor watches the auth directory and debounces change events
// into a single onChange call (macOS AuthDirectoryMonitor).
type DirectoryMonitor struct {
	debounce  time.Duration
	logPrefix string
	onChange  func()

	mu      sync.Mutex
	watcher *fsnotify.Watcher
	done    chan struct{}
	pending *time.Timer
	stopped bool
}

// NewDirectoryMonitor creates a monitor; call Start to begin watching.
func NewDirectoryMonitor(debounceInterval time.Duration, logPrefix string, onChange func()) *DirectoryMonitor {
	return &DirectoryMonitor{
		debounce:  debounceInterval,
		logPrefix: logPrefix,
		onChange:  onChange,
	}
}

// Start creates the auth directory if missing (the macOS app does the same so
// the monitor survives the directory being recreated) and begins watching.
// It returns an error only when the directory cannot be created or watched.
func (m *DirectoryMonitor) Start() error {
	m.Stop()

	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// The macOS app swallows this; keep watching if the directory
		// appears later is not possible without a parent watch, so log and
		// report the failure to the caller.
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return err
	}
	// Also watch the parent so a removed-and-recreated auth directory is
	// picked up again (the watch on the removed inode dies with it).
	if err := watcher.Add(filepath.Dir(dir)); err != nil {
		watcher.Close()
		return err
	}

	m.mu.Lock()
	m.watcher = watcher
	m.done = make(chan struct{})
	m.stopped = false
	done := m.done
	m.mu.Unlock()

	go m.loop(dir, watcher, done)
	return nil
}

// Stop stops watching and cancels any pending debounced call.
func (m *DirectoryMonitor) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	watcher := m.watcher
	m.watcher = nil
	done := m.done
	m.done = nil
	timer := m.pending
	m.pending = nil
	m.mu.Unlock()

	if timer != nil {
		timer.Stop()
	}
	if watcher != nil {
		watcher.Close()
	}
	if done != nil {
		<-done
	}
}

func (m *DirectoryMonitor) loop(dir string, watcher *fsnotify.Watcher, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-done:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// The auth directory was recreated: re-establish the watch.
			if event.Has(fsnotify.Create) && filepath.Clean(event.Name) == filepath.Clean(dir) {
				_ = watcher.Add(dir)
			}
			// Atomic writes (temp file + rename) surface as Create/Rename;
			// plain edits surface as Write. All are auth directory changes.
			m.scheduleRefresh()
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (m *DirectoryMonitor) scheduleRefresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	if m.pending != nil {
		m.pending.Stop()
	}
	m.pending = time.AfterFunc(m.debounce, func() {
		logx.Logf("%s Auth directory changed", m.logPrefix)
		m.onChange()
	})
}
