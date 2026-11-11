package journal

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

const (
	writerQueueSize = 256
	// lockWaitLimit bounds the wait for another writer's lock on a month
	// file; the record is dropped after it.
	lockWaitLimit = 500 * time.Millisecond
	// shutdownFlushLimit bounds how long close waits for queued records.
	shutdownFlushLimit = 2 * time.Second
	// diagnosticsInterval is the least time between drop and failure reports.
	diagnosticsInterval = time.Minute
)

var errLockTimeout = errors.New("journal file stayed locked by another writer")

type queuedLine struct {
	month string
	line  []byte
}

// recordWriter appends journal lines from one goroutine behind a bounded queue.
// enqueue never blocks or logs: a full queue drops the record, and drops and
// write failures are only counted there. The writer goroutine and close
// report them in aggregate.
type recordWriter struct {
	dir   string
	queue chan queuedLine
	done  chan struct{}

	mu     sync.RWMutex
	closed bool

	// abandoned is set when the shutdown flush deadline passes; the writer
	// goroutine then drops what it still dequeues.
	abandoned atomic.Bool

	dropped   atomic.Uint64
	failed    atomic.Uint64
	lastError atomic.Value

	reportMu        sync.Mutex
	reportedDropped uint64
	reportedFailed  uint64
	lastReport      time.Time
}

// newRecordWriter starts a writer for dir (created 0700 on first append).
func newRecordWriter(dir string, queueSize int) *recordWriter {
	w := &recordWriter{
		dir:   dir,
		queue: make(chan queuedLine, queueSize),
		done:  make(chan struct{}),
	}
	go w.run()
	return w
}

// enqueue queues rec for appending to its started_at month file.
func (w *recordWriter) enqueue(rec Record) {
	line, err := encodeLine(rec)
	if err != nil {
		w.countFailure(err)
		return
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return
	}
	select {
	case w.queue <- queuedLine{month: rec.StartedAt[:7], line: line}:
	default:
		w.dropped.Add(1)
	}
}

// close stops accepting records and waits for queued ones until the
// shutdown flush deadline; records still queued then are counted as dropped.
func (w *recordWriter) close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(shutdownFlushLimit):
		w.abandoned.Store(true)
		for range w.queue {
			w.dropped.Add(1)
		}
	}
	w.report(true)
}

func (w *recordWriter) countFailure(err error) {
	w.lastError.Store(err.Error())
	w.failed.Add(1)
}

func (w *recordWriter) run() {
	defer close(w.done)
	tick := time.NewTicker(diagnosticsInterval)
	defer tick.Stop()
	for {
		select {
		case q, ok := <-w.queue:
			if !ok {
				return
			}
			w.write(q)
		case <-tick.C:
		}
		w.report(false)
	}
}

func (w *recordWriter) write(q queuedLine) {
	if w.abandoned.Load() {
		w.dropped.Add(1)
		return
	}
	switch err := appendLine(w.dir, q.month, q.line); {
	case errors.Is(err, errLockTimeout):
		w.dropped.Add(1)
	case err != nil:
		w.countFailure(err)
	}
}

// report logs drops and write failures since the last report, at most once
// per diagnosticsInterval; the final report at close gives the totals.
func (w *recordWriter) report(final bool) {
	w.reportMu.Lock()
	defer w.reportMu.Unlock()
	dropped, failed := w.dropped.Load(), w.failed.Load()
	lastError, _ := w.lastError.Load().(string)
	if final {
		if dropped != 0 || failed != 0 {
			logx.Logf("[RequestJournal] Closed with %d records dropped and %d write failures in total (last error: %s)", dropped, failed, lastError)
		}
		w.reportedDropped, w.reportedFailed = dropped, failed
		return
	}
	if dropped == w.reportedDropped && failed == w.reportedFailed {
		return
	}
	if now := time.Now(); now.Sub(w.lastReport) >= diagnosticsInterval {
		logx.Logf("[RequestJournal] %d records dropped and %d write failures since the last report (last error: %s)",
			dropped-w.reportedDropped, failed-w.reportedFailed, lastError)
		w.reportedDropped, w.reportedFailed, w.lastReport = dropped, failed, now
	}
}

func monthFile(dir, month string) string {
	return filepath.Join(dir, "requests-"+month+".jsonl")
}

// appendLine appends one complete line under an exclusive advisory lock and
// fsyncs it, so writers in other processes never interleave with it.
func appendLine(dir, month string, line []byte) error {
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	f, created, err := openJournalFile(monthFile(dir, month))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockWithin(f, lockWaitLimit); err != nil {
		return err
	}
	if err := repairTornTail(f); err != nil {
		return err
	}
	// os.File.Write retries short writes until the whole line is written or
	// an error occurs; a partial line left by an error is repaired next time.
	if _, err := f.Write(line); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if created {
		return syncDir(dir)
	}
	return nil
}

// lockWithin takes an exclusive advisory lock on f, giving up after limit.
func lockWithin(f *os.File, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for delay := time.Millisecond; ; delay = min(2*delay, 50*time.Millisecond) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		if time.Now().After(deadline) {
			return errLockTimeout
		}
		time.Sleep(delay)
	}
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := checkOwner(info); err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// openJournalFile opens path for appending without following a symlink and
// checks that it is a regular file owned by this user.
func openJournalFile(path string) (*os.File, bool, error) {
	flags := os.O_RDWR | os.O_APPEND | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	f, err := os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, fs.ErrExist) {
		f, err = os.OpenFile(path, flags, 0)
	}
	if err != nil {
		return nil, false, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err == nil {
		err = checkOwner(info)
	}
	if err == nil && info.Mode().Perm() != 0o600 {
		err = f.Chmod(0o600)
	}
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return f, created, nil
}

func checkOwner(info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by this user", info.Name())
	}
	return nil
}

// repairTornTail truncates an incomplete last line left by a crash mid-append.
func repairTornTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	size := info.Size()
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	buf := make([]byte, 64<<10)
	end := size
	for end > 0 {
		start := max(end-int64(len(buf)), 0)
		chunk := buf[:end-start]
		if _, err := f.ReadAt(chunk, start); err != nil {
			return err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			end = start + int64(i) + 1
			break
		}
		end = start
	}
	logx.Logf("[RequestJournal] Removed %d bytes of an incomplete line from %s", size-end, f.Name())
	return f.Truncate(end)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
