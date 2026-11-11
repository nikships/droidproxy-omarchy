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
	w.enqueue(rec)
	deadline := time.Now().Add(5 * time.Second)
	for len(w.queue) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer never took the first record")
		}
		time.Sleep(time.Millisecond)
	}

	stop := captureStderr(t)
	for i := 0; i < 500; i++ {
		w.enqueue(rec)
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
