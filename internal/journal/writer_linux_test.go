//go:build linux

package journal

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// captureStderr redirects file descriptor 2, which logx writes to, until the
// returned function runs and returns what was written.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Dup3(int(w.Fd()), 2, 0); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(&out, r)
		close(copied)
	}()
	return func() string {
		_ = syscall.Dup3(saved, 2, 0)
		syscall.Close(saved)
		w.Close()
		<-copied
		r.Close()
		return out.String()
	}
}

func TestWriterDropDiagnosticsAreAggregatedOffTheCaller(t *testing.T) {
	dir := t.TempDir()
	path := monthFile(dir, "2026-11")
	release := holdLock(t, path)
	rec := testRecord("2026-11-11T10:00:00.000Z", "")

	w := newRecordWriter(dir, 1)
	w.enqueue(queued(rec))
	deadline := time.Now().Add(5 * time.Second)
	for len(w.queue) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer never took the first record")
		}
		time.Sleep(time.Millisecond)
	}

	stop := captureStderr(t)
	for i := 0; i < 500; i++ {
		w.enqueue(queued(rec))
	}
	if during := stop(); during != "" {
		t.Fatalf("enqueue wrote diagnostics on the caller: %d bytes, first line %q", len(during), strings.SplitN(during, "\n", 2)[0])
	}
	if got := w.dropped.Load(); got != 499 {
		t.Fatalf("dropped = %d, want 499", got)
	}

	release()
	stop = captureStderr(t)
	w.close()
	after := stop()
	if lines := strings.Count(after, "\n"); lines == 0 || lines > 3 || !strings.Contains(after, "499") {
		t.Fatalf("shutdown diagnostics = %q", after)
	}
	if lines := readLines(t, filepath.Join(dir, "requests-2026-11.jsonl")); len(lines) != 2 {
		t.Fatalf("%d lines written, want 2", len(lines))
	}
}

// stallStderr points file descriptor 2 at a pipe whose buffer is full and
// that nobody reads, so a write to it blocks. Cleanup restores descriptor 2
// and drains the pipe, which releases any blocked write.
func stallStderr(t *testing.T) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{4096, 1} {
		buf := make([]byte, size)
		for {
			if _, err := syscall.Write(fds[1], buf); err != nil {
				break
			}
		}
	}
	if err := syscall.SetNonblock(fds[1], false); err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Dup3(fds[1], 2, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Dup3(saved, 2, 0)
		syscall.Close(saved)
		syscall.Close(fds[1])
		go func() {
			buf := make([]byte, 64<<10)
			for {
				if n, err := syscall.Read(fds[0], buf); n <= 0 || err != nil {
					break
				}
			}
			syscall.Close(fds[0])
		}()
	})
}

func TestWriterCloseReturnsWithAStalledLogSink(t *testing.T) {
	dir := t.TempDir()
	// A directory where the month file belongs makes every append fail,
	// so the writer has failures to report.
	if err := os.Mkdir(monthFile(dir, "2026-11"), 0o700); err != nil {
		t.Fatal(err)
	}
	stallStderr(t)
	w := newRecordWriter(dir, 8)
	for i := 0; i < 3; i++ {
		w.enqueue(queued(testRecord("2026-11-11T10:00:00.000Z", "")))
	}
	start := time.Now()
	closed := make(chan struct{})
	go func() {
		w.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(shutdownFlushLimit + 2*time.Second):
		t.Fatalf("close still running %v after it started, with a stalled log sink", time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > shutdownFlushLimit+250*time.Millisecond {
		t.Fatalf("close took %v, past its %v deadline", elapsed, shutdownFlushLimit)
	}
	if got := w.failed.Load(); got != 3 {
		t.Fatalf("failed = %d, want 3", got)
	}
}

func TestWriterFinalDiagnosticCountsTheRecordInFlight(t *testing.T) {
	dir := t.TempDir()
	release := holdLock(t, monthFile(dir, "2026-11"))
	stop := captureStderr(t)
	w := newRecordWriter(dir, 16)
	for i := 0; i < 8; i++ {
		w.enqueue(queued(testRecord("2026-11-11T10:00:00.000Z", "")))
	}
	w.close()
	<-w.done
	<-w.logged
	log := stop()
	release()
	if !strings.Contains(log, "Closed with 8 records dropped") {
		t.Fatalf("final diagnostic does not count all 8 records: %q", log)
	}
	if got := w.dropped.Load(); got != 8 {
		t.Fatalf("dropped = %d, want 8", got)
	}
}
