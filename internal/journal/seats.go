package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// seatMissRefreshInterval limits directory rescans caused by unknown indexes.
	seatMissRefreshInterval = time.Second
	// maxSeatFileBytes: a larger auth file is not read, so it is not indexed.
	maxSeatFileBytes = 1 << 20
)

// seatFilePattern is the schema's pattern for seat.file.
var seatFilePattern = regexp.MustCompile(`^[^/\\]+\.json$`)

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
	// bindable: a regular file owned by this user whose identity was read
	// without the file changing underneath.
	bindable bool
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

// seatWork is one request's seat binding. It runs on its own goroutine,
// started when the trace names an auth index, so the relay never waits for
// the seat mutex or the auth directory; done is closed once it is set.
type seatWork struct {
	seats   *seatIndex
	index   string
	done    chan struct{}
	binding seatBinding
	bound   bool
}

func (s *seatIndex) startBind(index string) *seatWork {
	w := &seatWork{seats: s, index: index, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		w.binding, w.bound = s.bind(index, time.Now())
	}()
	return w
}

// identity revalidates a finished binding and returns its identity if it
// still holds. It runs after finalization, on the writer goroutine.
func (w *seatWork) identity() (seatIdentity, bool) {
	if !w.bound || !w.seats.stillBound(w.index, w.binding, time.Now()) {
		return seatIdentity{}, false
	}
	return w.binding.id, true
}

// bind resolves index to exactly one bindable auth file whose identity was
// read from the file as it is now.
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
	fp, bindableNow, err := lstatEntry(entry.path)
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
	if !entry.bindable || !bindableNow {
		return seatBinding{}, false
	}
	return seatBinding{path: entry.path, fp: entry.fp, id: entry.id}, true
}

// stillBound revalidates a binding after finalization: the index is still
// unique in the current map and names the same file, which still has the
// bound fingerprint, is still a regular file owned by this user, and is
// still readable.
func (s *seatIndex) stillBound(index string, b seatBinding, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stale {
		s.refreshLocked(now)
	}
	entries := s.byIndex[index]
	if len(entries) != 1 || entries[0].path != b.path {
		return false
	}
	if fp, bindable, err := lstatEntry(b.path); err != nil || fp != b.fp || !bindable {
		return false
	}
	f, err := openFileNoFollow(b.path, os.O_RDONLY, 0, true)
	if err != nil {
		return false
	}
	f.Close()
	return true
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
		if !strings.HasSuffix(de.Name(), ".json") {
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

// readSeatEntry reads the auth type and identity fields of one directory
// entry. Symlinks are read through, so their index resolves, but only a
// regular file owned by this user is bindable. Other non-regular entries are
// never opened.
func readSeatEntry(path string) (seatEntry, string, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return seatEntry{}, "", false
	}
	regular := info.Mode().IsRegular()
	if !regular && info.Mode()&fs.ModeSymlink == 0 {
		return seatEntry{}, "", false
	}
	var f *os.File
	if regular {
		f, err = openFileNoFollow(path, os.O_RDONLY, 0, true)
	} else {
		f, err = openFilePlain(path, os.O_RDONLY, 0, true)
	}
	if err != nil {
		return seatEntry{}, "", false
	}
	defer f.Close()
	if opened, err := f.Stat(); err != nil || !opened.Mode().IsRegular() || opened.Size() > maxSeatFileBytes {
		return seatEntry{}, "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSeatFileBytes+1))
	if err != nil || len(data) > maxSeatFileBytes {
		return seatEntry{}, "", false
	}
	fields, ok := objectMembers(data, "type", "email", "organization_uuid", "organization", "account_id")
	if !ok {
		return seatEntry{}, "", false
	}
	authType := jsonStringOr(fields["type"])
	if authType == "" {
		return seatEntry{}, "", false
	}

	before := fingerprintOf(info)
	after, err := lstatFingerprint(path)
	entry := seatEntry{
		path:     path,
		fp:       before,
		bindable: regular && ownedByMe(path, info) && err == nil && after == before,
	}
	if name := filepath.Base(path); seatFilePattern.MatchString(name) {
		entry.id.file = name
	}
	entry.id.email = jsonStringOr(fields["email"])
	switch strings.ToLower(authType) {
	case "claude":
		entry.id.org = jsonStringOr(fields["organization_uuid"])
		if entry.id.org == "" {
			nested, _ := objectMembers(fields["organization"], "uuid")
			entry.id.org = jsonStringOr(nested["uuid"])
		}
	case "codex":
		entry.id.accountID = jsonStringOr(fields["account_id"])
	}
	return entry, authType, true
}

// lstatEntry returns path's fingerprint and whether it is a regular file
// owned by this user. Ownership is not part of the fingerprint, since a
// chown changes neither size, mtime nor inode.
func lstatEntry(path string) (fingerprint, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fingerprint{}, false, err
	}
	return fingerprintOf(info), info.Mode().IsRegular() && ownedByMe(path, info), nil
}

func lstatFingerprint(path string) (fingerprint, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fingerprint{}, err
	}
	return fingerprintOf(info), nil
}

func fingerprintOf(info fs.FileInfo) fingerprint {
	return fingerprint{size: info.Size(), mtime: info.ModTime().UnixNano(), inode: fileInode(info)}
}

// objectMembers returns the raw values of the named top-level members of a
// strict JSON object, the last occurrence of each winning. Other members are
// skipped without being decoded. json.Valid accepts invalid UTF-8, so it is
// checked separately.
func objectMembers(data []byte, names ...string) (map[string]json.RawMessage, bool) {
	if !utf8.Valid(data) || !json.Valid(data) {
		return nil, false
	}
	i := skipSpace(data, 0)
	if data[i] != '{' {
		return nil, false
	}
	members := map[string]json.RawMessage{}
	i = skipSpace(data, i+1)
	for data[i] != '}' {
		keyEnd := skipString(data, i)
		var key string
		if json.Unmarshal(data[i:keyEnd], &key) != nil {
			return nil, false
		}
		i = skipSpace(data, skipSpace(data, keyEnd)+1)
		valueEnd := skipValue(data, i)
		if slices.Contains(names, key) {
			members[key] = data[i:valueEnd]
		}
		i = skipSpace(data, valueEnd)
		if data[i] == ',' {
			i = skipSpace(data, i+1)
		}
	}
	return members, true
}

// The skip helpers walk JSON already checked by json.Valid.

func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\r' || data[i] == '\n') {
		i++
	}
	return i
}

func skipString(data []byte, i int) int {
	for i++; data[i] != '"'; i++ {
		if data[i] == '\\' {
			i++
		}
	}
	return i + 1
}

func skipValue(data []byte, i int) int {
	switch data[i] {
	case '"':
		return skipString(data, i)
	case '{', '[':
		depth := 0
		for {
			switch data[i] {
			case '"':
				i = skipString(data, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			i++
			if depth == 0 {
				return i
			}
		}
	}
	for i < len(data) && !strings.ContainsRune(",}] \t\r\n", rune(data[i])) {
		i++
	}
	return i
}
