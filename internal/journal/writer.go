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

	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

const writerQueueSize = 256

type queuedLine struct {
	month string
	line  []byte
}

// recordWriter appends journal lines from one goroutine behind a bounded queue.
// enqueue never blocks: a full queue drops the record.
type recordWriter struct {
	dir   string
	queue chan queuedLine
	done  chan struct{}

	mu     sync.RWMutex
	closed bool

	dropped atomic.Uint64
	failed  atomic.Uint64
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
		w.countDrop()
		return
	}
	select {
	case w.queue <- queuedLine{month: rec.StartedAt[:7], line: line}:
	default:
		w.countDrop()
	}
}

// close stops accepting records and waits until queued ones are written.
func (w *recordWriter) close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *recordWriter) countDrop() {
	n := w.dropped.Add(1)
	logx.Logf("[RequestJournal] Queue full or closed; dropped a record (%d dropped)", n)
}

func (w *recordWriter) countFailure(err error) {
	n := w.failed.Add(1)
	logx.Logf("[RequestJournal] Could not write a record (%d failed): %v", n, err)
}

func (w *recordWriter) run() {
	defer close(w.done)
	for q := range w.queue {
		if err := appendLine(w.dir, q.month, q.line); err != nil {
			w.countFailure(err)
		}
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
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
