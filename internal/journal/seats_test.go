package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	o.feedAt(traceHead(index), false, time.Now())
	between()
	o.feedAt([]byte("{}"), false, time.Now())
	rec, _ := o.finishAt(EndEOF, time.Now())
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
