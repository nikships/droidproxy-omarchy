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
	// diagnosticsBacklog bounds the reports waiting for the log; a report
	// that finds it full is skipped and its counts go into the next one.
	diagnosticsBacklog = 8
)

var (
	errLockTimeout = errors.New("journal file stayed locked by another writer")
	errAbandoned   = errors.New("journal shutdown flush deadline passed")
)

// recordWriter appends journal lines from one goroutine behind a bounded queue.
// enqueue never blocks or logs: a full queue drops the record, and drops and
// write failures are only counted there. The writer goroutine and close
// report them in aggregate through a separate logging goroutine, so a log
// sink that stops reading never holds up appends or close.
type recordWriter struct {
	dir   string
	queue chan pending
	done  chan struct{}
	// abandon is closed when the shutdown flush deadline passes.
	abandon chan struct{}

	diagnostics chan string
	// logged is closed once every queued diagnostic has been logged.
	logged chan struct{}

	mu     sync.RWMutex
	closed bool

	// accepted counts queued records. settleMu orders the writer's outcome
	// for each record against the flush deadline, so every accepted record
	// is appended, failed, or dropped exactly once. settled counts the
	// outcomes the writer recorded; appending is set while it appends one.
	accepted  atomic.Uint64
	settleMu  sync.Mutex
	settled   uint64
	appending bool
	abandoned bool

	dropped   atomic.Uint64
	failed    atomic.Uint64
	lastError atomic.Value

	// reportMu guards the report state and the diagnostics channel; it is
	// never held while logging.
	reportMu        sync.Mutex
	reportedDropped uint64
	reportedFailed  uint64
	lastReport      time.Time
	diagnosticsShut bool
}

// newRecordWriter starts a writer for dir (created 0700 on first append).
func newRecordWriter(dir string, queueSize int) *recordWriter {
	w := &recordWriter{
		dir:         dir,
		queue:       make(chan pending, queueSize),
		done:        make(chan struct{}),
		abandon:     make(chan struct{}),
		diagnostics: make(chan string, diagnosticsBacklog),
		logged:      make(chan struct{}),
	}
	go w.run()
	go w.logDiagnostics()
	return w
}

// enqueue queues a finalized record for appending to its started_at month
// file.
func (w *recordWriter) enqueue(p pending) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return
	}
	select {
	case w.queue <- p:
		w.accepted.Add(1)
	default:
		w.dropped.Add(1)
	}
}

// close stops accepting records and waits for queued ones and their
// diagnostics until the shutdown flush deadline; records not yet appended
// then, queued or in flight, are counted as dropped. Only the first call
// does anything, so a record is never abandoned twice.
func (w *recordWriter) close() {
	deadline := time.Now().Add(shutdownFlushLimit)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.queue)
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(time.Until(deadline)):
		w.abandonRemaining()
	}
	w.report(true)
	select {
	case <-w.logged:
	case <-time.After(time.Until(deadline)):
	}
}

// abandonRemaining runs at the flush deadline. Every accepted record that
// the writer has neither settled nor started appending is counted as
// dropped, and the writer then drops whatever it holds or still dequeues.
func (w *recordWriter) abandonRemaining() {
	w.settleMu.Lock()
	defer w.settleMu.Unlock()
	w.abandoned = true
	unsettled := w.accepted.Load() - w.settled
	if w.appending {
		unsettled--
	}
	w.dropped.Add(unsettled)
	close(w.abandon)
}

// startAppend reports whether the writer may append its current record;
// after it returns true the deadline no longer drops that record.
func (w *recordWriter) startAppend() bool {
	w.settleMu.Lock()
	defer w.settleMu.Unlock()
	if w.abandoned {
		return false
	}
	w.appending = true
	return true
}

// settle records the writer's outcome for its current record, counted by
// count, unless the flush deadline already counted the record as dropped.
func (w *recordWriter) settle(count func()) {
	w.settleMu.Lock()
	defer w.settleMu.Unlock()
	if w.abandoned && !w.appending {
		return
	}
	w.appending = false
	w.settled++
	if count != nil {
		count()
	}
}

func (w *recordWriter) logDiagnostics() {
	defer close(w.logged)
	for msg := range w.diagnostics {
		logx.Logf("%s", msg)
	}
}

// diagnose queues msg for the log without waiting; it is skipped when the
// backlog is full or closed.
func (w *recordWriter) diagnose(msg string) {
	w.reportMu.Lock()
	defer w.reportMu.Unlock()
	w.diagnoseLocked(msg)
}

// diagnoseLocked reports whether msg was queued.
func (w *recordWriter) diagnoseLocked(msg string) bool {
	if w.diagnosticsShut {
		return false
	}
	select {
	case w.diagnostics <- msg:
		return true
	default:
		return false
	}
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

func (w *recordWriter) write(p pending) {
	select {
	case <-w.abandon:
		return
	default:
	}
	rec, ok := p.record(w.abandon)
	if !ok {
		return
	}
	line, err := encodeLine(rec)
	if err != nil {
		w.settle(func() { w.countFailure(err) })
		return
	}
	removed, err := appendLine(w.dir, rec.StartedAt[:7], line, w.abandon, w.startAppend)
	switch {
	case errors.Is(err, errAbandoned):
	case errors.Is(err, errLockTimeout):
		w.settle(func() { w.dropped.Add(1) })
	case err != nil:
		w.settle(func() { w.countFailure(err) })
	default:
		w.settle(nil)
	}
	if removed > 0 {
		w.diagnose(fmt.Sprintf("[RequestJournal] Removed %d bytes of an incomplete line from %s", removed, monthFile(w.dir, rec.StartedAt[:7])))
	}
}

// report queues a log line for drops and write failures since the last
// report, at most once per diagnosticsInterval; the final report at close
// gives the totals and closes the diagnostics backlog.
func (w *recordWriter) report(final bool) {
	w.reportMu.Lock()
	defer w.reportMu.Unlock()
	if w.diagnosticsShut {
		return
	}
	dropped, failed := w.dropped.Load(), w.failed.Load()
	lastError, _ := w.lastError.Load().(string)
	if final {
		if dropped != 0 || failed != 0 {
			w.diagnoseLocked(fmt.Sprintf("[RequestJournal] Closed with %d records dropped and %d write failures in total (last error: %s)", dropped, failed, lastError))
		}
		w.diagnosticsShut = true
		close(w.diagnostics)
		return
	}
	if dropped == w.reportedDropped && failed == w.reportedFailed {
		return
	}
	now := time.Now()
	if now.Sub(w.lastReport) < diagnosticsInterval {
		return
	}
	if w.diagnoseLocked(fmt.Sprintf("[RequestJournal] %d records dropped and %d write failures since the last report (last error: %s)",
		dropped-w.reportedDropped, failed-w.reportedFailed, lastError)) {
		w.reportedDropped, w.reportedFailed, w.lastReport = dropped, failed, now
	}
}

func monthFile(dir, month string) string {
	return filepath.Join(dir, "requests-"+month+".jsonl")
}

// appendLine appends one complete line under an exclusive advisory lock and
// fsyncs it, so writers in other processes never interleave with it. It
// returns how many bytes of an incomplete last line it removed first. It
// gives up with errAbandoned when stop is closed during the lock wait or
// start returns false once the lock is held; the file is unchanged then.
func appendLine(dir, month string, line []byte, stop <-chan struct{}, start func() bool) (removed int64, err error) {
	if err := ensurePrivateDir(dir); err != nil {
		return 0, err
	}
	f, created, err := openJournalFile(monthFile(dir, month))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := lockWithin(f, lockWaitLimit, stop); err != nil {
		return 0, err
	}
	if !start() {
		return 0, errAbandoned
	}
	if removed, err = repairTornTail(f); err != nil {
		return removed, err
	}
	// os.File.Write retries short writes until the whole line is written or
	// an error occurs; a partial line left by an error is repaired next time.
	if _, err := f.Write(line); err != nil {
		return removed, err
	}
	if err := f.Sync(); err != nil {
		return removed, err
	}
	if created {
		return removed, syncDir(dir)
	}
	return removed, nil
}

// lockWithin takes an exclusive advisory lock on f, giving up after limit
// or when stop is closed.
func lockWithin(f *os.File, limit time.Duration, stop <-chan struct{}) error {
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
		select {
		case <-stop:
			return errAbandoned
		case <-time.After(delay):
		}
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

// repairTornTail truncates an incomplete last line left by a crash
// mid-append and returns how many bytes it removed.
func repairTornTail(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0, err
	}
	size := info.Size()
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return 0, nil
	}
	buf := make([]byte, 64<<10)
	end := size
	for end > 0 {
		start := max(end-int64(len(buf)), 0)
		chunk := buf[:end-start]
		if _, err := f.ReadAt(chunk, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			end = start + int64(i) + 1
			break
		}
		end = start
	}
	if err := f.Truncate(end); err != nil {
		return 0, err
	}
	return size - end, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
