package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/claude"
)

// seatMissRefreshInterval limits directory rescans caused by unknown indexes.
const seatMissRefreshInterval = time.Second

type fingerprint struct {
	size  int64
	mtime int64
	inode uint64
}

type seatIdentity struct {
	file, email, org, accountID string
}

type seatEntry struct {
	path string
	fp   fingerprint
	id   seatIdentity
	// readable is false when the file changed while its identity was read.
	readable bool
}

// seatBinding is the identity bound to one request and the fingerprint of
// the file it was read from.
type seatBinding struct {
	path string
	fp   fingerprint
	id   seatIdentity
}

// seatIndex maps CLIProxyAPI auth indexes to auth files in the auth
// directory, reading only identity fields.
type seatIndex struct {
	dir string

	mu          sync.Mutex
	byIndex     map[string][]seatEntry
	stale       bool
	lastRefresh time.Time
}

func newSeatIndex(dir string) *seatIndex {
	return &seatIndex{dir: dir, stale: true}
}

// invalidate makes the next lookup rescan the auth directory.
func (s *seatIndex) invalidate() {
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
}

// authIndex is CLIProxyAPI's stable auth index for a file-backed auth.
func authIndex(authType, path string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(authType) + ":" + path))
	return hex.EncodeToString(sum[:8])
}

// bind resolves index to exactly one auth file whose identity was read
// from the file as it is now.
func (s *seatIndex) bind(index string, now time.Time) (seatBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		s.refreshLocked(now)
	}
	entries, hit := s.byIndex[index]
	if !hit && now.Sub(s.lastRefresh) >= seatMissRefreshInterval {
		s.refreshLocked(now)
		entries = s.byIndex[index]
	}
	if len(entries) != 1 {
		return seatBinding{}, false
	}
	entry := entries[0]
	fp, err := statFingerprint(entry.path)
	if err != nil {
		s.stale = true
		return seatBinding{}, false
	}
	if fp != entry.fp {
		fresh, authType, ok := readSeatEntry(entry.path)
		if !ok || authIndex(authType, entry.path) != index {
			s.stale = true
			return seatBinding{}, false
		}
		entry = fresh
		entries[0] = fresh
	}
	if !entry.readable {
		return seatBinding{}, false
	}
	return seatBinding{path: entry.path, fp: entry.fp, id: entry.id}, true
}

// stillBound reports whether the bound file is unchanged.
func (b seatBinding) stillBound() bool {
	fp, err := statFingerprint(b.path)
	return err == nil && fp == b.fp
}

func (s *seatIndex) refreshLocked(now time.Time) {
	s.stale = false
	s.lastRefresh = now
	s.byIndex = map[string][]seatEntry{}
	dir, err := filepath.Abs(s.dir)
	if err != nil {
		return
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range names {
		if de.IsDir() || !strings.EqualFold(filepath.Ext(de.Name()), ".json") {
			continue
		}
		path := filepath.Join(dir, de.Name())
		entry, authType, ok := readSeatEntry(path)
		if !ok {
			continue
		}
		index := authIndex(authType, path)
		s.byIndex[index] = append(s.byIndex[index], entry)
	}
}

// readSeatEntry reads the auth type and identity fields of one auth file.
// Other members, including tokens, are never decoded.
func readSeatEntry(path string) (seatEntry, string, bool) {
	before, err := statFingerprint(path)
	if err != nil {
		return seatEntry{}, "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return seatEntry{}, "", false
	}
	var fields struct {
		Type             json.RawMessage `json:"type"`
		Email            json.RawMessage `json:"email"`
		OrganizationUUID json.RawMessage `json:"organization_uuid"`
		Organization     json.RawMessage `json:"organization"`
		AccountID        json.RawMessage `json:"account_id"`
	}
	if json.Unmarshal(data, &fields) != nil {
		return seatEntry{}, "", false
	}
	authType, _ := jsonString(fields.Type)
	if authType == "" {
		return seatEntry{}, "", false
	}
	after, err := statFingerprint(path)
	entry := seatEntry{path: path, fp: before, readable: err == nil && after == before}

	entry.id.file = filepath.Base(path)
	if strings.Contains(entry.id.file, `\`) {
		entry.id.file = ""
	}
	entry.id.email = trimmedString(fields.Email)
	switch strings.ToLower(authType) {
	case "claude":
		orgFields := map[string]any{}
		if v, ok := jsonString(fields.OrganizationUUID); ok {
			orgFields["organization_uuid"] = v
		}
		if org := decodeAny(fields.Organization); org != nil {
			orgFields["organization"] = org
		}
		entry.id.org = claude.OrganizationFieldsFrom(orgFields).UUID
	case "codex":
		entry.id.accountID = trimmedString(fields.AccountID)
	}
	return entry, authType, true
}

func trimmedString(raw json.RawMessage) string {
	s, _ := jsonString(raw)
	return strings.TrimSpace(s)
}

func decodeAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

func statFingerprint(path string) (fingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fingerprint{}, err
	}
	fp := fingerprint{size: info.Size(), mtime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		fp.inode = uint64(st.Ino)
	}
	return fp, nil
}
