package journal

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAuthIndexFormula(t *testing.T) {
	// Expected values computed independently with
	// printf '%s' 'claude:/home/u/.cli-proxy-api/claude-a@b.com.json' | shasum -a 256 | cut -c1-16
	if got := authIndex("Claude", "/home/u/.cli-proxy-api/claude-a@b.com.json"); got != "73d11bcd0c70fd6f" {
		t.Fatalf("claude index = %s", got)
	}
	if got := authIndex("codex", "/home/u/.cli-proxy-api/codex-x.json"); got != "202bfd23632a692c" {
		t.Fatalf("codex index = %s", got)
	}
}

func writeAuthFile(t *testing.T, dir, name string, content map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(content)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(path)
	return abs
}

func TestSeatBindReadsIdentityFieldsOnly(t *testing.T) {
	dir := t.TempDir()
	claudePath := writeAuthFile(t, dir, "claude-seat@example.invalid-org.json", map[string]any{
		"type": "claude", "email": "seat@example.invalid", "access_token": "synthetic",
		"organization": map[string]any{"uuid": "org-nested", "name": "Example Org"},
	})
	codexPath := writeAuthFile(t, dir, "codex-acct-seat@example.invalid.json", map[string]any{
		"type": "codex", "email": "seat@example.invalid", "account_id": "acct-1", "organization_uuid": "ignored",
	})
	geminiPath := writeAuthFile(t, dir, "gemini-seat.json", map[string]any{"type": "gemini", "account_id": "ignored"})
	writeAuthFile(t, dir, "untyped.json", map[string]any{"email": "nobody@example.invalid"})

	seats := newSeatIndex(dir)
	now := time.Now()
	for _, c := range []struct {
		path, authType string
		want           seatIdentity
	}{
		{claudePath, "claude", seatIdentity{file: "claude-seat@example.invalid-org.json", email: "seat@example.invalid", org: "org-nested"}},
		{codexPath, "codex", seatIdentity{file: "codex-acct-seat@example.invalid.json", email: "seat@example.invalid", accountID: "acct-1"}},
		{geminiPath, "gemini", seatIdentity{file: "gemini-seat.json"}},
	} {
		b, ok := seats.bind(authIndex(c.authType, c.path), now)
		if !ok || b.id != c.want {
			t.Errorf("%s: bound=%v id=%+v, want %+v", c.path, ok, b.id, c.want)
		}
	}
}

func TestSeatMissRefreshIsRateLimited(t *testing.T) {
	dir := t.TempDir()
	seats := newSeatIndex(dir)
	t0 := time.Now()
	if _, ok := seats.bind("0123456789abcdef", t0); ok {
		t.Fatal("empty directory bound a seat")
	}
	path := writeAuthFile(t, dir, "claude-new.json", map[string]any{"type": "claude", "email": "new@example.invalid"})
	index := authIndex("claude", path)
	if _, ok := seats.bind(index, t0.Add(500*time.Millisecond)); ok {
		t.Fatal("miss rescanned within one second")
	}
	if _, ok := seats.bind(index, t0.Add(time.Second)); !ok {
		t.Fatal("miss after one second did not rescan")
	}

	other := writeAuthFile(t, dir, "claude-other.json", map[string]any{"type": "claude", "email": "other@example.invalid"})
	seats.invalidate()
	if _, ok := seats.bind(authIndex("claude", other), t0.Add(time.Second+time.Millisecond)); !ok {
		t.Fatal("watcher invalidation did not rescan")
	}
}

func traceHead(index string) []byte {
	return []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nX-CPA-TRACE-ID: 20261111100000-" + index +
		"-019a7140-0000-7000-8000-000000000001\r\nContent-Length: 2\r\n\r\n")
}

func observeSeat(t *testing.T, j *Journal, index string, between func()) *Seat {
	t.Helper()
	o := j.begin(RequestContext{Method: "POST", Path: "/v1/messages", ForwardedPath: "/v1/messages", Head: "POST /v1/messages HTTP/1.1"}, time.Now())
	deliver(o, traceHead(index), time.Now(), time.Now())
	if o.seat != nil {
		<-o.seat.done
	}
	between()
	if rec, ok := deliver(o, []byte("{}"), time.Now(), time.Now()); ok {
		return rec.Seat
	}
	rec, _ := finish(o, EndEOF, time.Now())
	return rec.Seat
}

func TestSeatChangedMidRequestKeepsOnlyIndexAndRole(t *testing.T) {
	dir := t.TempDir()
	path := writeAuthFile(t, dir, "claude-seat.json", map[string]any{"type": "claude", "email": "before@example.invalid"})
	index := authIndex("claude", path)
	j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(dir)}

	seat := observeSeat(t, j, index, func() {})
	if seat == nil || seat.Email != "before@example.invalid" || seat.File != "claude-seat.json" {
		t.Fatalf("unchanged file: seat = %+v", seat)
	}
	seat = observeSeat(t, j, index, func() {
		writeAuthFile(t, dir, "claude-seat.json", map[string]any{"type": "claude", "email": "replacement@example.invalid"})
	})
	if seat == nil || *seat != (Seat{AuthIndex: index, Role: roleServed}) {
		t.Fatalf("file replaced mid-request: seat = %+v", seat)
	}
	seat = observeSeat(t, j, index, func() { os.Remove(path) })
	if seat == nil || *seat != (Seat{AuthIndex: index, Role: roleServed}) {
		t.Fatalf("file removed mid-request: seat = %+v", seat)
	}
}

func TestSeatDuplicateIndexIsUnresolved(t *testing.T) {
	dir := t.TempDir()
	a := writeAuthFile(t, dir, "claude-a.json", map[string]any{"type": "claude", "email": "a@example.invalid"})
	b := writeAuthFile(t, dir, "claude-b.json", map[string]any{"type": "claude", "email": "b@example.invalid"})
	seats := newSeatIndex(dir)
	seats.mu.Lock()
	seats.refreshLocked(time.Now())
	index := authIndex("claude", a)
	seats.byIndex[index] = append(seats.byIndex[index], seats.byIndex[authIndex("claude", b)]...)
	seats.mu.Unlock()

	j := &Journal{enabled: func() bool { return true }, seats: seats}
	seat := observeSeat(t, j, index, func() {})
	if seat == nil || *seat != (Seat{AuthIndex: index, Role: roleServed}) {
		t.Fatalf("duplicate index: seat = %+v", seat)
	}
}

func TestSeatIndexFileRules(t *testing.T) {
	dir := t.TempDir()
	identity := map[string]any{"type": "claude", "email": "seat@example.invalid", "organization_uuid": "org-1"}
	regular := writeAuthFile(t, dir, "claude-regular.json", identity)
	target := writeAuthFile(t, t.TempDir(), "elsewhere.json", identity)
	link := filepath.Join(dir, "claude-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	upper := writeAuthFile(t, dir, "claude-upper.JSON", identity)
	backslash := writeAuthFile(t, dir, `claude-back\slash.json`, identity)
	bare := writeAuthFile(t, dir, ".json", identity)
	fifo := filepath.Join(dir, "claude-fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(dir)}

	only := func(path string) Seat { return Seat{AuthIndex: authIndex("claude", path), Role: roleServed} }
	bound := func(path, file string) Seat {
		return Seat{AuthIndex: authIndex("claude", path), Role: roleServed, File: file, Email: "seat@example.invalid", Org: "org-1"}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, c := range []struct {
			name, path string
			want       Seat
		}{
			{"regular file", regular, bound(regular, "claude-regular.json")},
			{"symlink", link, only(link)},
			{"uppercase extension", upper, only(upper)},
			{"basename failing the file pattern", backslash, bound(backslash, "")},
			{"basename that is only the extension", bare, bound(bare, "")},
			{"fifo", fifo, only(fifo)},
		} {
			seat := observeSeat(t, j, c.want.AuthIndex, func() {})
			if seat == nil || *seat != c.want {
				t.Errorf("%s: seat = %+v, want %+v", c.name, seat, c.want)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("seat lookup blocked on a non-regular file")
	}
}

func TestSeatOwnedByAnotherUserIsNotBound(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("creating a file owned by another user needs root")
	}
	dir := t.TempDir()
	path := writeAuthFile(t, dir, "claude-foreign.json", map[string]any{"type": "claude", "email": "seat@example.invalid"})
	if err := os.Chown(path, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(dir)}
	index := authIndex("claude", path)
	if seat := observeSeat(t, j, index, func() {}); seat == nil || *seat != (Seat{AuthIndex: index, Role: roleServed}) {
		t.Fatalf("foreign-owned file: seat = %+v", seat)
	}
}

func TestSeatIdentityReadsListedMembersOnly(t *testing.T) {
	dir := t.TempDir()
	claudePath := writeAuthFile(t, dir, "claude-case.json", map[string]any{
		"type": "CLAUDE", "Email": "wrong@example.invalid", "ORGANIZATION_UUID": "wrong-org",
		"organization": map[string]any{"uuid": "org-nested", "name": "Example Org"},
	})
	codexPath := writeAuthFile(t, dir, "codex-case.json", map[string]any{
		"type": "codex", "email": "seat@example.invalid", "Account_ID": "wrong-account",
	})
	j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(dir)}
	for _, c := range []struct {
		path, authType string
		want           Seat
	}{
		{claudePath, "claude", Seat{File: "claude-case.json", Org: "org-nested"}},
		{codexPath, "codex", Seat{File: "codex-case.json", Email: "seat@example.invalid"}},
	} {
		c.want.AuthIndex, c.want.Role = authIndex(c.authType, c.path), roleServed
		if seat := observeSeat(t, j, c.want.AuthIndex, func() {}); seat == nil || *seat != c.want {
			t.Errorf("%s: seat = %+v, want %+v", c.path, seat, c.want)
		}
	}
}

func TestClaudeOrgPrecedence(t *testing.T) {
	for i, c := range []struct {
		fields map[string]any
		org    string
	}{
		{map[string]any{"organization_uuid": "top", "organization": map[string]any{"uuid": "nested"}}, "top"},
		{map[string]any{"organization": map[string]any{"uuid": "nested"}, "organization_uuid": "top"}, "top"},
		{map[string]any{"organization_uuid": "", "organization": map[string]any{"uuid": "nested"}}, "nested"},
		{map[string]any{"organization_uuid": 7, "organization": map[string]any{"uuid": "nested"}}, "nested"},
		{map[string]any{"organization": map[string]any{"organization_uuid": "other"}}, ""},
		{map[string]any{"organization": map[string]any{"uuid": ""}}, ""},
		{map[string]any{"organization": "org-string"}, ""},
	} {
		dir := t.TempDir()
		c.fields["type"] = "claude"
		path := writeAuthFile(t, dir, fmt.Sprintf("claude-%d.json", i), c.fields)
		j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(dir)}
		want := Seat{AuthIndex: authIndex("claude", path), Role: roleServed, File: filepath.Base(path), Org: c.org}
		if seat := observeSeat(t, j, want.AuthIndex, func() {}); seat == nil || *seat != want {
			t.Errorf("%v: seat = %+v, want org %q", c.fields, seat, c.org)
		}
	}
}

func TestSeatRevalidatedAtFinalization(t *testing.T) {
	dir := t.TempDir()
	identity := map[string]any{"type": "claude", "email": "seat@example.invalid"}
	j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(dir)}

	if os.Getuid() != 0 {
		path := writeAuthFile(t, dir, "claude-unreadable.json", identity)
		index := authIndex("claude", path)
		seat := observeSeat(t, j, index, func() {
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
		})
		if seat == nil || *seat != (Seat{AuthIndex: index, Role: roleServed}) {
			t.Errorf("file unreadable at finalization: seat = %+v", seat)
		}
	}

	path := writeAuthFile(t, dir, "claude-duplicated.json", identity)
	other := writeAuthFile(t, dir, "claude-other.json", identity)
	index := authIndex("claude", path)
	j.seats.invalidate()
	if seat := observeSeat(t, j, index, func() {}); seat == nil || seat.File != "claude-duplicated.json" {
		t.Fatalf("unique index before the change: seat = %+v", seat)
	}
	seat := observeSeat(t, j, index, func() {
		j.seats.mu.Lock()
		j.seats.byIndex[index] = append(j.seats.byIndex[index], j.seats.byIndex[authIndex("claude", other)]...)
		j.seats.mu.Unlock()
	})
	if seat == nil || *seat != (Seat{AuthIndex: index, Role: roleServed}) {
		t.Errorf("index no longer unique at finalization: seat = %+v", seat)
	}
}

// indexedPaths lists the paths the seat index holds after a fresh scan.
func indexedPaths(s *seatIndex) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(time.Now())
	paths := map[string]bool{}
	for _, entries := range s.byIndex {
		for _, e := range entries {
			paths[e.path] = true
		}
	}
	return paths
}

// authFileOfSize writes a claude auth file of exactly size bytes.
func authFileOfSize(t *testing.T, dir, name string, size int) string {
	t.Helper()
	prefix := `{"type":"claude","email":"seat@example.invalid","pad":"`
	data := prefix + strings.Repeat("p", size-len(prefix)-2) + `"}`
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(path)
	return abs
}

func TestSeatFileSizeLimit(t *testing.T) {
	dir := t.TempDir()
	atLimit := authFileOfSize(t, dir, "claude-at-limit.json", 1<<20)
	over := authFileOfSize(t, dir, "claude-over.json", 1<<20+1)
	bigTarget := authFileOfSize(t, t.TempDir(), "big.json", 1<<20+1)
	link := filepath.Join(dir, "claude-big-link.json")
	if err := os.Symlink(bigTarget, link); err != nil {
		t.Fatal(err)
	}
	seats := newSeatIndex(dir)
	indexed := indexedPaths(seats)
	if !indexed[atLimit] || indexed[over] || indexed[link] {
		t.Fatalf("indexed %v", indexed)
	}
	j := &Journal{enabled: func() bool { return true }, seats: seats}
	if seat := observeSeat(t, j, authIndex("claude", atLimit), func() {}); seat == nil || seat.File != "claude-at-limit.json" {
		t.Errorf("file of exactly 1 MiB: seat = %+v", seat)
	}
	for _, path := range []string{over, link} {
		index := authIndex("claude", path)
		if seat := observeSeat(t, j, index, func() {}); seat == nil || *seat != (Seat{AuthIndex: index, Role: roleServed}) {
			t.Errorf("%s: seat = %+v", path, seat)
		}
	}
}

func TestNonRegularEntriesAreNotIndexed(t *testing.T) {
	// A short directory keeps the socket path within the platform limit.
	dir, err := os.MkdirTemp("/tmp", "seats")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	regular := writeAuthFile(t, dir, "claude-regular.json", map[string]any{"type": "claude"})
	fifo := filepath.Join(dir, "f.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s.json")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fifoAbs, _ := filepath.Abs(fifo)
	socketAbs, _ := filepath.Abs(socket)
	done := make(chan map[string]bool)
	go func() { done <- indexedPaths(newSeatIndex(dir)) }()
	select {
	case indexed := <-done:
		if !indexed[regular] || indexed[fifoAbs] || indexed[socketAbs] || len(indexed) != 1 {
			t.Fatalf("indexed %v", indexed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scan blocked on a non-regular entry")
	}
}

// chownKeepingFingerprint changes path's owner and checks that size, mtime,
// and inode stay the same, so only the owner tells the files apart.
func chownKeepingFingerprint(t *testing.T, path string, uid int) {
	t.Helper()
	before, err := lstatFingerprint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, uid); err != nil {
		t.Fatal(err)
	}
	if after, _ := lstatFingerprint(path); after != before {
		t.Fatalf("fingerprint changed from %+v to %+v", before, after)
	}
}

func TestSeatOwnerChangeWithSameFingerprintIsNotBound(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changing a file's owner needs root")
	}
	dir := t.TempDir()
	path := writeAuthFile(t, dir, "claude-owned.json", map[string]any{"type": "claude", "email": "seat@example.invalid"})
	index := authIndex("claude", path)
	only := Seat{AuthIndex: index, Role: roleServed}
	j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(dir)}

	if seat := observeSeat(t, j, index, func() {}); seat == nil || seat.Email != "seat@example.invalid" {
		t.Fatalf("own file: seat = %+v", seat)
	}
	chownKeepingFingerprint(t, path, 65534)
	if seat := observeSeat(t, j, index, func() {}); seat == nil || *seat != only {
		t.Errorf("cached entry after an owner change: seat = %+v", seat)
	}
	chownKeepingFingerprint(t, path, 0)
	if seat := observeSeat(t, j, index, func() {}); seat == nil || seat.Email != "seat@example.invalid" {
		t.Fatalf("owner changed back: seat = %+v", seat)
	}
	if seat := observeSeat(t, j, index, func() { chownKeepingFingerprint(t, path, 65534) }); seat == nil || *seat != only {
		t.Errorf("owner changed between binding and finalization: seat = %+v", seat)
	}
}

// relayHead and relayFinal are the relay's two journal calls that a seat
// affects: the head that names the seat, and the buffer that finalizes the
// record. Each returns how long the call took.
func relayHead(j *Journal, index string) (*Observation, time.Duration) {
	o := j.Begin(requestFor("/v1/messages", `{"model":"claude-x"}`))
	start := time.Now()
	o.Delivered(traceHead(index), time.Now())
	return o, time.Since(start)
}

func relayFinal(o *Observation) time.Duration {
	start := time.Now()
	o.Delivered([]byte("{}"), time.Now())
	return time.Since(start)
}

// withinOrTimeout runs call on its own goroutine and reports false if it
// has not returned after a second.
func withinOrTimeout(call func()) bool {
	done := make(chan struct{})
	go func() {
		call()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func TestSeatWorkStaysOffTheRelay(t *testing.T) {
	dir := t.TempDir()
	var index string
	for i := 0; i < 64; i++ {
		path := authFileOfSize(t, dir, fmt.Sprintf("claude-%03d.json", i), 1<<20)
		if i == 0 {
			index = authIndex("claude", path)
		}
	}
	j := New(filepath.Join(t.TempDir(), "journal"), dir, func() bool { return true })
	defer j.Close()
	const limit = time.Millisecond

	o, head := relayHead(j, index)
	if final := relayFinal(o); head > limit || final > limit {
		t.Errorf("cold scan of 64 credential files of 1 MiB: head took %v, finalization %v", head, final)
	}

	// A refresh holds the seat mutex while it reads every credential file.
	refreshing, release := make(chan struct{}), make(chan struct{})
	go func() {
		j.seats.mu.Lock()
		close(refreshing)
		j.seats.refreshLocked(time.Now())
		<-release
		j.seats.mu.Unlock()
	}()
	<-refreshing
	defer close(release)

	var final time.Duration
	if !withinOrTimeout(func() { o, head = relayHead(j, index); final = relayFinal(o) }) {
		t.Fatal("head and finalization waited for a refresh holding the seat mutex")
	}
	if head > limit || final > limit {
		t.Errorf("during a refresh: head took %v, finalization %v", head, final)
	}
}
