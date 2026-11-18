package wg

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrStatusUnknown means the dump file does not exist (yet): the status job
// has not run, so nothing is known about handshakes.
var ErrStatusUnknown = errors.New("wg: status unknown (no dump file)")

// PeerStatus is one line of `wg show wg0 dump` as written by the status timer
// (public key, endpoint, allowed IPs, last handshake, rx, tx).
type PeerStatus struct {
	PublicKey     string
	Endpoint      string
	AllowedIPs    string
	LastHandshake time.Time // zero = never
	RxBytes       uint64
	TxBytes       uint64
}

// HandshakeAge is how long ago the last handshake was; ok is false if there
// has never been one.
func (p PeerStatus) HandshakeAge(now time.Time) (time.Duration, bool) {
	if p.LastHandshake.IsZero() {
		return 0, false
	}
	return now.Sub(p.LastHandshake), true
}

// ParseDump reads the tab-separated status lines, keyed by public key.
// Malformed lines are skipped; an error is returned only for read failures.
func ParseDump(r io.Reader) (map[string]PeerStatus, error) {
	out := map[string]PeerStatus{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		f := strings.Split(strings.TrimSpace(sc.Text()), "\t")
		if len(f) < 6 || !ValidKey(f[0]) {
			continue
		}
		hs, _ := strconv.ParseInt(f[3], 10, 64)
		rx, _ := strconv.ParseUint(f[4], 10, 64)
		tx, _ := strconv.ParseUint(f[5], 10, 64)
		ps := PeerStatus{PublicKey: f[0], Endpoint: f[1], AllowedIPs: f[2], RxBytes: rx, TxBytes: tx}
		if ps.Endpoint == "(none)" {
			ps.Endpoint = ""
		}
		if hs > 0 {
			ps.LastHandshake = time.Unix(hs, 0)
		}
		out[f[0]] = ps
	}
	return out, sc.Err()
}

// ReadDump parses the dump file at path and returns the time it was written.
// A missing file returns ErrStatusUnknown.
func ReadDump(path string) (map[string]PeerStatus, time.Time, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, ErrStatusUnknown
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	m, err := ParseDump(f)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read %s: %w", path, err)
	}
	return m, fi.ModTime(), nil
}
