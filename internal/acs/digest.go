package acs

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTP Digest authentication, RFC 7616 with the MD5 algorithm (RFC 2617 form): that is what CPEs speak.
// The server side is stateless apart from a small replay table (nonce count per nonce).

const (
	realm      = "ftn-acs"
	nonceLife  = 5 * time.Minute
	maxNonces  = 4096
	opaqueText = "ftn"
)

type digestAuth struct {
	key []byte // HMAC key of the nonces; random per process (a restart makes the CPE answer a fresh challenge)
	now func() time.Time

	mu     sync.Mutex
	counts map[string]int64 // nonce -> highest nc seen
}

func newDigestAuth(now func() time.Time) *digestAuth {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return &digestAuth{key: k, now: now, counts: map[string]int64{}}
}

func (a *digestAuth) nonce() string {
	ts := strconv.FormatInt(a.now().Unix(), 16)
	r := make([]byte, 8)
	_, _ = rand.Read(r)
	body := ts + "." + hex.EncodeToString(r)
	return base64.RawURLEncoding.EncodeToString([]byte(body + "." + a.mac(body)))
}

func (a *digestAuth) mac(body string) string {
	h := hmac.New(sha256.New, a.key)
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// nonceOK checks the HMAC and the age of a nonce.
func (a *digestAuth) nonceOK(n string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return false
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 {
		return false
	}
	body := parts[0] + "." + parts[1]
	if subtle.ConstantTimeCompare([]byte(a.mac(body)), []byte(parts[2])) != 1 {
		return false
	}
	ts, err := strconv.ParseInt(parts[0], 16, 64)
	if err != nil {
		return false
	}
	age := a.now().Sub(time.Unix(ts, 0))
	return age >= -time.Minute && age <= nonceLife
}

// challenge writes the 401 with a fresh nonce.
func (a *digestAuth) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", algorithm=MD5, nonce="%s", opaque="%s"`, realm, a.nonce(), opaqueText))
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// parseDigest splits an Authorization header ("Digest a=b, c="d"") into its fields.
func parseDigest(h string) map[string]string {
	h = strings.TrimSpace(h)
	if len(h) < 7 || !strings.EqualFold(h[:7], "digest ") {
		return nil
	}
	out := map[string]string{}
	s := h[7:]
	for len(s) > 0 {
		s = strings.TrimLeft(s, ", \t")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		k := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var v string
		if strings.HasPrefix(s, `"`) {
			end := 1
			var b strings.Builder
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' && end+1 < len(s) {
					end++
				}
				b.WriteByte(s[end])
				end++
			}
			v = b.String()
			if end < len(s) {
				end++
			}
			s = s[end:]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			v = strings.TrimSpace(s[:end])
			s = s[end:]
		}
		if len(out) < 16 {
			out[k] = v
		}
	}
	return out
}

// digestResponse is the response value of RFC 2617/7616 (MD5).
func digestResponse(user, pass, method, uri, nonce, nc, cnonce, qop string) string {
	ha1 := md5hex(user + ":" + realm + ":" + pass)
	ha2 := md5hex(method + ":" + uri)
	if qop == "" {
		return md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	return md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
}

// verify checks the Authorization header of r against user/pass. A header with a stale nonce says stale.
func (a *digestAuth) verify(r *http.Request, user, pass string) (ok bool) {
	f := parseDigest(r.Header.Get("Authorization"))
	if f == nil || user == "" {
		return false
	}
	if f["realm"] != realm || (f["algorithm"] != "" && !strings.EqualFold(f["algorithm"], "MD5")) {
		return false
	}
	if !a.nonceOK(f["nonce"]) {
		return false
	}
	// The URI in the header must be the URI of the request (an Authorization header cannot be replayed on another path).
	if f["uri"] != r.RequestURI && f["uri"] != r.URL.Path && f["uri"] != r.URL.String() {
		return false
	}
	qop := f["qop"]
	if qop != "" && qop != "auth" {
		return false
	}
	want := digestResponse(user, pass, r.Method, f["uri"], f["nonce"], f["nc"], f["cnonce"], qop)
	userOK := subtle.ConstantTimeCompare([]byte(f["username"]), []byte(user)) == 1
	respOK := subtle.ConstantTimeCompare([]byte(strings.ToLower(f["response"])), []byte(want)) == 1
	if !userOK || !respOK {
		return false
	}
	if qop == "auth" {
		// nc must go up for a nonce (replay of a captured request is refused). CPEs that send nc=00000001 on every
		// request after a new challenge are fine: each challenge makes a new nonce.
		nc, err := strconv.ParseInt(f["nc"], 16, 64)
		if err != nil || nc < 1 {
			return false
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if last, seen := a.counts[f["nonce"]]; seen && nc <= last {
			return false
		}
		if len(a.counts) >= maxNonces {
			a.counts = map[string]int64{} // a flood of nonces: forget them all (a replay then needs a fresh nonce window)
		}
		a.counts[f["nonce"]] = nc
	}
	return true
}

// clientDigest answers a WWW-Authenticate: Digest challenge for a GET (connection requests to a CPE).
func clientDigest(challenge, user, pass, method, uri string) (string, bool) {
	f := parseDigest(challenge)
	if f == nil || f["nonce"] == "" {
		return "", false
	}
	if a := f["algorithm"]; a != "" && !strings.EqualFold(a, "MD5") {
		return "", false
	}
	rlm := f["realm"]
	cn := make([]byte, 8)
	_, _ = rand.Read(cn)
	cnonce := hex.EncodeToString(cn)
	qop := ""
	for _, q := range strings.Split(f["qop"], ",") {
		if strings.TrimSpace(q) == "auth" {
			qop = "auth"
		}
	}
	ha1 := md5hex(user + ":" + rlm + ":" + pass)
	ha2 := md5hex(method + ":" + uri)
	var resp string
	if qop == "" {
		resp = md5hex(ha1 + ":" + f["nonce"] + ":" + ha2)
	} else {
		resp = md5hex(ha1 + ":" + f["nonce"] + ":00000001:" + cnonce + ":auth:" + ha2)
	}
	h := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`, user, rlm, f["nonce"], uri, resp)
	if qop != "" {
		h += fmt.Sprintf(`, qop=auth, nc=00000001, cnonce="%s"`, cnonce)
	}
	if o := f["opaque"]; o != "" {
		h += fmt.Sprintf(`, opaque="%s"`, o)
	}
	return h, true
}
