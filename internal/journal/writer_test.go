package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testRecord(startedAt, model string) Record {
	rec := Record{
		V:           1,
		ExecutionID: "019a7140-0000-7000-8000-000000000001",
		StartedAt:   startedAt,
		EndedAt:     startedAt,
		Origin:      "client",
		Endpoint:    "/v1/messages",
		Provider:    "claude",
		Outcome:     outcomeComplete,
	}
	if model != "" {
		rec.RequestedModel = &model
	}
	return rec
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		t.Fatalf("%s does not end with LF", path)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestWriterAppendsToPrivateMonthFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	w := newRecordWriter(dir, 8)
	rec := testRecord("2026-11-30T23:59:59.999Z", "claude-haiku-4-5-20251001")
	w.enqueue(queued(rec))
	w.close()

	path := filepath.Join(dir, "requests-2026-11.jsonl")
	want, _ := encodeLine(rec)
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatalf("file = %q, want %q", got, want)
	}
	for p, mode := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %v, want %v (%v)", p, info.Mode().Perm(), mode, err)
		}
	}
	if w.dropped.Load() != 0 || w.failed.Load() != 0 {
		t.Fatalf("dropped=%d failed=%d", w.dropped.Load(), w.failed.Load())
	}
}

func TestWriterRefusesSymlinkAndNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, monthFile(dir, "2026-11")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(monthFile(dir, "2026-12"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := newRecordWriter(dir, 8)
	w.enqueue(queued(testRecord("2026-11-11T10:00:00.000Z", "")))
	w.enqueue(queued(testRecord("2026-12-11T10:00:00.000Z", "")))
	w.close()
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Fatalf("wrote through a symlink: %q", data)
	}
	if w.failed.Load() != 2 {
		t.Fatalf("failed = %d, want 2", w.failed.Load())
	}
}

func TestWriterRepairsTornTail(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := monthFile(dir, "2026-11")
	complete := `{"v":1,"complete":true}` + "\n"
	if err := os.WriteFile(path, []byte(complete+`{"v":1,"torn`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := testRecord("2026-11-11T10:00:00.000Z", "")
	if _, err := appendLine(dir, "2026-11", mustLine(t, rec), nil, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got, want := string(mustRead(t, path)), complete+string(mustLine(t, rec)); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}

	if err := os.WriteFile(path, []byte(`{"only":"torn`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := appendLine(dir, "2026-11", mustLine(t, rec), nil, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, path)); got != string(mustLine(t, rec)) {
		t.Fatalf("file = %q", got)
	}
}

// Two writers on separate file handles stand in for two processes: flock
// locks belong to the open file description on Linux and macOS.
func TestConcurrentWritersNeverInterleave(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("m", 256<<10)
	const perWriter = 64
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			writer := newRecordWriter(dir, perWriter)
			for i := 0; i < perWriter; i++ {
				writer.enqueue(queued(testRecord("2026-11-11T10:00:00.000Z", big+string(rune('a'+w)))))
			}
			writer.close()
		}(w)
	}
	wg.Wait()

	f, err := os.Open(monthFile(dir, "2026-11"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	count := 0
	for scanner.Scan() {
		var rec Record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("line %d is not one record: %v", count, err)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2*perWriter {
		t.Fatalf("%d lines, want %d", count, 2*perWriter)
	}
}

func TestWriterQueueOverflowDropsWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := monthFile(dir, "2026-11")
	held, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := tryFlock(held); err != nil {
		t.Fatal(err)
	}

	w := newRecordWriter(dir, 2)
	rec := testRecord("2026-11-11T10:00:00.000Z", "")
	w.enqueue(queued(rec))
	waitTaken(t, w)
	start := time.Now()
	for i := 0; i < 7; i++ {
		w.enqueue(queued(rec))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("enqueue blocked for %v", elapsed)
	}
	if w.dropped.Load() != 5 {
		t.Fatalf("dropped = %d, want 5", w.dropped.Load())
	}

	if err := held.Close(); err != nil { // closing releases the lock
		t.Fatal(err)
	}
	w.close()
	if lines := readLines(t, path); len(lines) != 3 {
		t.Fatalf("%d lines written, want 3", len(lines))
	}
	w.enqueue(queued(rec))
	if w.dropped.Load() != 6 {
		t.Fatalf("enqueue after Close was not dropped")
	}
}

func TestWriterEnqueueDuringCloseDropsWithoutWaiting(t *testing.T) {
	w := newRecordWriter(t.TempDir(), 2)
	w.mu.Lock()
	returned := make(chan struct{})
	go func() {
		w.enqueue(queued(testRecord("2026-11-11T10:00:00.000Z", "")))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		w.mu.Unlock()
		<-returned
		t.Fatal("enqueue waited for close")
	}
	w.mu.Unlock()
	if w.dropped.Load() != 1 || w.accepted.Load() != 0 {
		t.Fatalf("dropped = %d, accepted = %d; want 1, 0", w.dropped.Load(), w.accepted.Load())
	}
	w.close()
}

// queued is rec as the writer receives it from a finalized observation
// whose request fields are already known.
func queued(rec Record) pending { return pending{rec: rec} }

func mustLine(t *testing.T, rec Record) []byte {
	t.Helper()
	line, err := encodeLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// holdLock takes the journal's advisory lock on path the way another
// process would, until the returned release runs.
func holdLock(t *testing.T, path string) func() {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryFlock(held); err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() {
		held.Close() // closing releases the lock
	})
	t.Cleanup(release)
	return release
}

func TestWriterCloseDoesNotWaitOnAHeldLock(t *testing.T) {
	dir := t.TempDir()
	path := monthFile(dir, "2026-11")
	holdLock(t, path)

	w := newRecordWriter(dir, 16)
	for i := 0; i < 8; i++ {
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
	case <-time.After(6 * time.Second):
		t.Fatal("close waited on a journal lock held elsewhere")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("close took %v", elapsed)
	}
	if got := w.dropped.Load(); got != 8 {
		t.Fatalf("dropped when close returned = %d, want 8", got)
	}
	<-w.done
	if got := w.dropped.Load(); got != 8 {
		t.Fatalf("dropped = %d, want 8", got)
	}
	if data := mustRead(t, path); len(data) != 0 {
		t.Fatalf("wrote while the lock was held: %q", data)
	}
}

// waitTaken waits until the writer goroutine has taken every queued record.
func waitTaken(t *testing.T, w *recordWriter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(w.queue) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer never took the queued records")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWriterCloseDropsARecordStillWaitingForItsRequest(t *testing.T) {
	dir := t.TempDir()
	w := newRecordWriter(dir, 1)
	parse := &requestFacts{done: make(chan struct{})}
	w.enqueue(pending{rec: testRecord("2026-11-11T10:00:00.000Z", ""), request: parse})
	waitTaken(t, w)
	w.close()
	if got := w.dropped.Load(); got != 1 {
		t.Errorf("dropped when close returned = %d, want 1", got)
	}
	close(parse.done)
	<-w.done
	if got := w.dropped.Load(); got != 1 {
		t.Errorf("dropped after the parse finished = %d, want 1", got)
	}
	if data, err := os.ReadFile(monthFile(dir, "2026-11")); err == nil && len(data) != 0 {
		t.Errorf("appended after close: %q", data)
	}
}

func TestCloseDropsARecordStillWaitingForSeatWork(t *testing.T) {
	authDir := t.TempDir()
	path := writeAuthFile(t, authDir, "claude-seat.json", map[string]any{"type": "claude", "email": "seat@example.invalid"})
	dir := filepath.Join(t.TempDir(), "journal")
	j := New(dir, authDir, func() bool { return true })
	j.seats.mu.Lock()
	unlock := sync.OnceFunc(j.seats.mu.Unlock)
	t.Cleanup(unlock)

	var o *Observation
	if !withinOrTimeout(func() {
		o, _ = relayHead(j, authIndex("claude", path))
		relayFinal(o)
	}) {
		t.Fatal("the relay waited for seat work")
	}
	j.Close()
	if got := j.writer.dropped.Load(); got != 1 {
		t.Errorf("dropped when close returned = %d, want 1", got)
	}
	unlock()
	<-j.writer.done
	if got := j.writer.dropped.Load(); got != 1 {
		t.Errorf("dropped after the seat work finished = %d, want 1", got)
	}
	if data, err := os.ReadFile(monthFile(dir, o.started.UTC().Format("2006-01"))); err == nil && len(data) != 0 {
		t.Errorf("appended after close: %q", data)
	}
}

// A second Close while a record is still waiting on seat revalidation must
// neither count that record again nor close the abandon channel twice.
func TestCloseTwiceCountsAnAbandonedRecordOnce(t *testing.T) {
	dir := t.TempDir()
	path := writeAuthFile(t, dir, "claude-test.json", map[string]any{"type": "claude", "email": "test@example.invalid"})
	j := New(filepath.Join(t.TempDir(), "journal"), dir, func() bool { return true })
	o := j.Begin(requestFor("/v1/messages", `{"model":"claude-x"}`))
	o.Delivered(traceHead(authIndex("claude", path)), time.Now())
	<-o.seat.done
	j.seats.mu.Lock()
	o.Delivered([]byte("{}"), time.Now())
	waitTaken(t, j.writer)
	j.Close()
	first := j.writer.dropped.Load()
	start := time.Now()
	j.Close()
	second := time.Since(start)
	j.seats.mu.Unlock()
	<-j.writer.done
	if first != 1 || j.writer.dropped.Load() != 1 {
		t.Errorf("dropped after first Close %d, after second %d; want 1 and 1", first, j.writer.dropped.Load())
	}
	if second > 100*time.Millisecond {
		t.Errorf("second Close took %v; want it to return at once", second)
	}
}
