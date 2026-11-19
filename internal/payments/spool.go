package payments

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ispbill/internal/store"
)

// A Spool keeps raw M-Pesa callback bodies on disk when the database cannot
// take the payment. Daraja is always answered "Accepted", so without a spool
// a failed save would lose the payment. Replaying is safe to repeat: the
// receipt is the ledger key, so a payment that was saved after all is a no-op.
type Spool struct {
	dir       string
	mu        sync.Mutex
	replaying sync.Mutex // one Replay at a time
	seq       int64
}

// NewSpool uses dir (created on first use).
func NewSpool(dir string) *Spool { return &Spool{dir: dir} }

// ErrKeep is what a Replay handler returns for a file that is not finished yet
// and must stay in the spool, without blocking the files behind it.
var ErrKeep = errors.New("spool: not finished yet")

// IsPersistError tells whether err means "the database did not take it".
func IsPersistError(err error) bool {
	return errors.Is(err, store.ErrPersist) || errors.Is(err, store.ErrUnavailable)
}

// Put writes a callback body as DIR/<unixnano>-<seq>-<kind>.json (mode 0600).
// The time comes first and is zero-padded, so a plain name sort is oldest
// first. (Older versions wrote <kind>-<unixnano>-<seq>.json; Replay still reads
// those.)
func (s *Spool) Put(kind string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	s.seq++
	name := fmt.Sprintf("%019d-%06d-%s.json", time.Now().UnixNano(), s.seq, kind)
	return store.WriteFileAtomic(filepath.Join(s.dir, name), body)
}

// Pending counts waiting files.
func (s *Spool) Pending() int {
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.json"))
	return len(files)
}

type spoolFile struct {
	path, kind string
	at, seq    int64
}

// parseSpoolName reads both name formats: <unixnano>-<seq>-<kind>.json and the
// old <kind>-<unixnano>-<seq>.json.
func parseSpoolName(path string) spoolFile {
	f := spoolFile{path: path}
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	parts := strings.SplitN(base, "-", 3)
	num := func(v string) (int64, bool) { n, err := strconv.ParseInt(v, 10, 64); return n, err == nil }
	if len(parts) == 3 {
		if at, ok := num(parts[0]); ok {
			if seq, ok := num(parts[1]); ok {
				f.at, f.seq, f.kind = at, seq, parts[2]
				return f
			}
		}
		if at, ok := num(parts[1]); ok {
			if seq, ok := num(parts[2]); ok {
				f.at, f.seq, f.kind = at, seq, parts[0]
				return f
			}
		}
	}
	f.kind, _, _ = strings.Cut(base, "-")
	return f
}

// list returns the waiting files, oldest first.
func (s *Spool) list() []spoolFile {
	s.mu.Lock()
	paths, _ := filepath.Glob(filepath.Join(s.dir, "*.json"))
	s.mu.Unlock()
	files := make([]spoolFile, len(paths))
	for i, p := range paths {
		files[i] = parseSpoolName(p)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].at != files[j].at {
			return files[i].at < files[j].at
		}
		if files[i].seq != files[j].seq {
			return files[i].seq < files[j].seq
		}
		return files[i].path < files[j].path
	})
	return files
}

// Replay feeds every waiting body (oldest first) to handle. A file is removed
// when handle returns nil; replay stops at the first persist error, since the
// database is still not taking writes. It returns how many were applied.
// The spool lock is held only to list and to rename or remove a file, never
// while handle runs (a router push can take seconds), so callbacks can still
// be spooled meanwhile. Only one Replay runs at a time; another call while one
// is running returns at once.
func (s *Spool) Replay(handle func(kind string, body []byte) error) (int, error) {
	if !s.replaying.TryLock() {
		return 0, nil
	}
	defer s.replaying.Unlock()
	done := 0
	for _, f := range s.list() {
		body, err := os.ReadFile(f.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return done, err
		}
		if err := handle(f.kind, body); err != nil {
			if errors.Is(err, ErrKeep) {
				continue // not finished (a receipt still being confirmed): look again next time
			}
			if IsPersistError(err) {
				return done, err
			}
			// A body that cannot be applied for another reason would block the
			// queue forever: set it aside for a human.
			s.mu.Lock()
			_ = os.Rename(f.path, f.path+".bad")
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		err = os.Remove(f.path)
		s.mu.Unlock()
		if err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}
