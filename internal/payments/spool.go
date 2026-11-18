package payments

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	dir string
	mu  sync.Mutex
	seq int64
}

// NewSpool uses dir (created on first use).
func NewSpool(dir string) *Spool { return &Spool{dir: dir} }

// IsPersistError tells whether err means "the database did not take it".
func IsPersistError(err error) bool {
	return errors.Is(err, store.ErrPersist) || errors.Is(err, store.ErrUnavailable)
}

// Put writes a callback body as DIR/<kind>-<unixnano>.json (mode 0600).
func (s *Spool) Put(kind string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	s.seq++
	name := fmt.Sprintf("%s-%d-%d.json", kind, time.Now().UnixNano(), s.seq)
	return store.WriteFileAtomic(filepath.Join(s.dir, name), body)
}

// Pending counts waiting files.
func (s *Spool) Pending() int {
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.json"))
	return len(files)
}

// Replay feeds every waiting body (oldest first) to handle. A file is removed
// when handle returns nil; replay stops at the first persist error, since the
// database is still not taking writes. It returns how many were applied.
func (s *Spool) Replay(handle func(kind string, body []byte) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.json"))
	sort.Strings(files)
	done := 0
	for _, f := range files {
		kind, _, _ := strings.Cut(filepath.Base(f), "-")
		body, err := os.ReadFile(f)
		if err != nil {
			return done, err
		}
		if err := handle(kind, body); err != nil {
			if IsPersistError(err) {
				return done, err
			}
			// A body that cannot be applied for another reason would block the
			// queue forever: set it aside for a human.
			_ = os.Rename(f, f+".bad")
			continue
		}
		if err := os.Remove(f); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}
