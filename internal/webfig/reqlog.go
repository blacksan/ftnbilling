package webfig

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Request log of the WebFig host. One line per request, with nothing secret in
// it: the method, a path class (never a query string: /_ftn/open carries the
// ticket), the status, the bytes sent, how long it took, how long it waited for
// a slot, the router's site ID, the upstream error and the browser's protocol
// (Caddy passes it as X-Ftn-Proto; the hop from Caddy to the app is always
// HTTP/1.1). No cookies, no bodies, no client address beyond what the admin
// log already has. A page load is 30 to 50 requests, so plain lines are
// limited per window; lines that matter (errors, 503s, slow, long waits) have
// a larger allowance of their own, and the number dropped is reported.
const (
	logWindow      = 10 * time.Second
	logPlainPerWin = 20
	logNotePerWin  = 100
	logSlow        = 3 * time.Second
	logSlowWait    = 500 * time.Millisecond
)

type reqRecord struct {
	start time.Time
	wait  time.Duration
	site  string
	note  string
	upErr error
}

type logWriter struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (l *logWriter) WriteHeader(code int) {
	if l.code == 0 {
		l.code = code
	}
	l.ResponseWriter.WriteHeader(code)
}

func (l *logWriter) Write(b []byte) (int, error) {
	if l.code == 0 {
		l.code = http.StatusOK
	}
	n, err := l.ResponseWriter.Write(b)
	l.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController (the reverse proxy's flushes) reach the real writer.
func (l *logWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }

type reqLimiter struct {
	mu                 sync.Mutex
	winStart           time.Time
	plain, noted, drop int
}

// allow says whether a line may be written now, and how many were dropped since the last one written.
func (q *reqLimiter) allow(now time.Time, important bool) (ok bool, dropped int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if now.Sub(q.winStart) >= logWindow {
		q.winStart, q.plain, q.noted = now, 0, 0
	}
	if important && q.noted < logNotePerWin {
		q.noted++
	} else if !important && q.plain < logPlainPerWin {
		q.plain++
	} else {
		q.drop++
		return false, 0
	}
	dropped, q.drop = q.drop, 0
	return true, dropped
}

var fileName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

// pathClass names the kind of request without anything that could carry a secret.
func pathClass(p string) string {
	switch {
	case p == "/", p == "/webfig", p == "/webfig/", p == "/webfig/list", p == "/jsproxy", p == "/_ftn/open", p == "/favicon.ico":
		return p
	case strings.HasPrefix(p, "/jsproxy/"):
		return "/jsproxy/*"
	case strings.HasPrefix(p, "/webfig/"):
		if f := strings.TrimPrefix(p, "/webfig/"); fileName.MatchString(f) {
			return p
		}
		return "/webfig/*"
	case strings.HasPrefix(p, "/graphs/"):
		return "/graphs/*"
	}
	return "other"
}

func (m *Manager) logRequest(r *http.Request, lw *logWriter, rec *reqRecord) {
	if m.o.Log == nil {
		return
	}
	dur := time.Since(rec.start)
	code := lw.code
	if code == 0 {
		code = http.StatusOK // what net/http sends for a handler that wrote nothing
	}
	important := code >= 400 || rec.upErr != nil || rec.note != "" ||
		rec.wait >= logSlowWait || (dur >= logSlow && !isJSProxy(r))
	ok, dropped := m.rl.allow(time.Now(), important)
	if !ok {
		return
	}
	proto := r.Header.Get("X-Ftn-Proto") // set by Caddy (header_up); absent when it is not configured
	if len(proto) > 16 {
		proto = proto[:16]
	}
	attrs := []any{"method", r.Method, "path", pathClass(r.URL.Path), "status", code, "bytes", lw.bytes,
		"ms", dur.Milliseconds(), "wait_ms", rec.wait.Milliseconds(), "site", rec.site,
		"proto", proto, "hop", r.Proto, "fetch", r.Header.Get("Sec-Fetch-Mode")}
	if rec.note != "" {
		attrs = append(attrs, "note", rec.note)
	}
	if rec.upErr != nil {
		e := rec.upErr.Error()
		if errors.Is(rec.upErr, context.Canceled) {
			e = "canceled (client went away)"
		}
		if len(e) > 200 {
			e = e[:200]
		}
		attrs = append(attrs, "upstream_err", e)
	}
	if dropped > 0 {
		attrs = append(attrs, "dropped_before", dropped)
	}
	if important {
		m.o.Log.Warn("webfig", attrs...)
		return
	}
	m.o.Log.Info("webfig", attrs...)
}
