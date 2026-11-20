package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// The JSON API for the app: https://<host>/api/v1, documented in docs/api-v1.md.
// Bearer tokens only (cookies are never read here), one error envelope, and
// Idempotency-Key replay for every state-changing call.

// apiCtx is what a handler knows about its caller.
type apiCtx struct {
	user store.AdminUser
	dev  store.APIDevice
	ip   string
}

type apiFunc func(w http.ResponseWriter, r *http.Request, c *apiCtx)

// Error codes of the envelope.
const (
	codeBadRequest   = "bad_request"
	codeValidation   = "validation_failed"
	codeInvalidCreds = "invalid_credentials"
	codeTOTPInvalid  = "totp_invalid"
	codeTokenExpired = "token_expired"
	codeTokenRevoked = "token_revoked"
	codeEnrol        = "totp_enrol_required"
	codeForbidden    = "forbidden_role"
	codeNotFound     = "not_found"
	codeMACInUse     = "mac_in_use"
	codeFlatInUse    = "flat_in_use"
	codeDupPayment   = "duplicate_payment"
	codeLowerPlan    = "lower_plan_active"
	codeBadState     = "bad_state"
	codeRateLimited  = "rate_limited"
	codeDBDown       = "db_unavailable"
	codeInternal     = "internal"
)

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"could not encode the answer","details":{}}}`)
	}
	b = addRouterAliases(b)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func apiErr(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	writeJSON(w, status, map[string]any{"error": apiError{code, msg, details}})
}

func validationErr(w http.ResponseWriter, fields map[string]string) {
	f := map[string]any{}
	for k, v := range fields {
		f[k] = v
	}
	apiErr(w, http.StatusBadRequest, codeValidation, "Some fields need fixing.", map[string]any{"fields": f})
}

func rateLimited(w http.ResponseWriter, wait time.Duration, busy bool) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	msg := "Too many attempts. Wait a few minutes and try again."
	if busy {
		msg = "The server is busy. Try again in a moment."
	}
	apiErr(w, http.StatusTooManyRequests, codeRateLimited, msg, map[string]any{"retry_after_s": secs})
}

// decode reads a JSON body into v. An empty body is fine when optional.
func decode(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		apiErr(w, http.StatusBadRequest, codeBadRequest, "The request body could not be read.", nil)
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		if optional {
			return true
		}
		apiErr(w, http.StatusBadRequest, codeBadRequest, "A JSON body is required.", nil)
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		apiErr(w, http.StatusBadRequest, codeBadRequest, "The body is not valid JSON for this call.", nil)
		return false
	}
	return true
}

// ---- idempotency ---------------------------------------------------------------------------

const idemTTL = 24 * time.Hour

type idemEntry struct {
	fp     string // method, path and body hash
	done   chan struct{}
	status int
	body   []byte
	at     time.Time
}

// idemStore keeps the answers to calls that carried an Idempotency-Key, per
// device, for 24 hours. It lives in memory: after a restart a retried call
// runs again (the calls are safe to repeat: duplicates are refused by their own
// rules, such as the one-MAC rule and unique payment codes).
type idemStore struct {
	mu sync.Mutex
	m  map[string]*idemEntry
}

func newIdemStore() *idemStore { return &idemStore{m: map[string]*idemEntry{}} }

const (
	idemMax       = 20000
	idemPerDevice = 500 // one phone cannot fill the table for the others
)

// begin returns the entry for key and whether the caller owns it (must run the
// call and finish it); otherwise the caller waits on e.done and replays.
func (s *idemStore) begin(key, fp string, now time.Time) (e *idemEntry, owner bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.m[key]; ok && now.Sub(old.at) < idemTTL {
		return old, false
	}
	if dev, _, ok := strings.Cut(key, "|"); ok {
		s.capDevice(dev+"|", now)
	}
	if len(s.m) >= idemMax {
		for k, v := range s.m {
			if now.Sub(v.at) >= idemTTL {
				delete(s.m, k)
			}
		}
		if len(s.m) >= idemMax {
			s.m = map[string]*idemEntry{}
		}
	}
	e = &idemEntry{fp: fp, done: make(chan struct{}), at: now}
	s.m[key] = e
	return e, true
}

// capDevice makes room for one more entry of the device whose keys start with
// prefix: expired ones go first, then the oldest.
func (s *idemStore) capDevice(prefix string, now time.Time) {
	n := 0
	for k, v := range s.m {
		if strings.HasPrefix(k, prefix) {
			if now.Sub(v.at) >= idemTTL {
				delete(s.m, k)
			} else {
				n++
			}
		}
	}
	for ; n >= idemPerDevice; n-- {
		oldest, ok := "", false
		var at time.Time
		for k, v := range s.m {
			if strings.HasPrefix(k, prefix) && (!ok || v.at.Before(at)) {
				oldest, at, ok = k, v.at, true
			}
		}
		if !ok {
			return
		}
		delete(s.m, oldest)
	}
}

func (s *idemStore) drop(key string, e *idemEntry) {
	s.mu.Lock()
	if s.m[key] == e {
		delete(s.m, key)
	}
	s.mu.Unlock()
}

// recorder captures what a handler answered.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(c int) {
	if r.status == 0 {
		r.status = c
		r.ResponseWriter.WriteHeader(c)
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if r.buf.Len() < 1<<20 {
		r.buf.Write(b)
	}
	return r.ResponseWriter.Write(b)
}

// ---- the wrapper -----------------------------------------------------------------------------

// api authenticates the bearer token, checks the role and runs h. Writes honour
// Idempotency-Key.
func (s *Server) api(min string, h apiFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.fromTunnel(r) {
			http.NotFound(w, r)
			return
		}
		tok := bearer(r)
		if tok == "" {
			apiErr(w, http.StatusUnauthorized, codeTokenRevoked, "Sign in again.", nil)
			return
		}
		dev, user, err := s.authn.AuthAccess(tok)
		switch {
		case errors.Is(err, authn.ErrTokenExpired):
			apiErr(w, http.StatusUnauthorized, codeTokenExpired, "The access token expired. Refresh it.", nil)
			return
		case err != nil:
			apiErr(w, http.StatusUnauthorized, codeTokenRevoked, "This sign-in is no longer valid. Sign in again.", nil)
			return
		}
		if authn.RoleRank(user.Role) < authn.RoleRank(min) {
			apiErr(w, http.StatusForbidden, codeForbidden, "Your role cannot do this.", map[string]any{"role": user.Role, "needs": min})
			return
		}
		ip := realIP(r)
		s.authn.TouchDevice(dev.ID, ip)
		c := &apiCtx{user: user, dev: dev, ip: ip}
		aliasRequest(r) // ?router= and router_id mean site (D8)
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			h(w, r, c)
			return
		}
		s.idempotent(w, r, c, h)
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, c *apiCtx, h apiFunc) {
	if idemExempt(r) { // the answer holds a one-time secret: never stored, never replayed
		h(w, r, c)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		h(w, r, c)
		return
	}
	if len(key) > 100 {
		apiErr(w, http.StatusBadRequest, codeBadRequest, "The Idempotency-Key is too long.", nil)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		apiErr(w, http.StatusBadRequest, codeBadRequest, "The request body could not be read.", nil)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	sum := sha256.Sum256(body)
	fp := r.Method + " " + r.URL.Path + " " + hex.EncodeToString(sum[:])
	full := c.dev.ID + "|" + key
	e, owner := s.idem.begin(full, fp, time.Now())
	if !owner {
		select {
		case <-e.done:
		case <-time.After(30 * time.Second):
			apiErr(w, http.StatusConflict, codeBadState, "The first request with this Idempotency-Key is still running.", nil)
			return
		case <-r.Context().Done():
			return
		}
		if e.fp != fp {
			apiErr(w, http.StatusBadRequest, codeBadRequest, "This Idempotency-Key was used for a different request.", nil)
			return
		}
		if e.status == 0 { // the first run was dropped (server error): run again
			s.idempotent(w, r, c, h)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(e.status)
		_, _ = w.Write(e.body)
		return
	}
	rec := &recorder{ResponseWriter: w}
	defer func() {
		if rv := recover(); rv != nil {
			s.idem.drop(full, e)
			close(e.done)
			panic(rv)
		}
		// Only definite answers are kept; a server error may be transient.
		if rec.status != 0 && rec.status < 500 {
			e.status, e.body = rec.status, append([]byte(nil), rec.buf.Bytes()...)
		} else {
			s.idem.drop(full, e)
		}
		close(e.done)
	}()
	h(rec, r, c)
}

// ---- pagination ------------------------------------------------------------------------------

type page struct{ limit, offset int }

func parsePage(w http.ResponseWriter, r *http.Request) (page, bool) {
	p := page{limit: 50}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			validationErr(w, map[string]string{"limit": "must be a whole number from 1 to 200"})
			return p, false
		}
		if n > 200 {
			n = 200
		}
		p.limit = n
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		b, err := base64.RawURLEncoding.DecodeString(v)
		n, err2 := strconv.Atoi(strings.TrimPrefix(string(b), "o:"))
		if err != nil || err2 != nil || !strings.HasPrefix(string(b), "o:") || n < 0 {
			validationErr(w, map[string]string{"cursor": "is not a cursor from this server"})
			return p, false
		}
		p.offset = n
	}
	return p, true
}

func cursorFor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

// slicePage cuts items to the page and returns the next cursor (nil = last page).
func slicePage[T any](items []T, p page) (out []T, next any) {
	if p.offset >= len(items) {
		return []T{}, nil
	}
	end := p.offset + p.limit
	if end >= len(items) {
		return items[p.offset:], nil
	}
	return items[p.offset:end], cursorFor(end)
}

func pageJSON[T any](w http.ResponseWriter, items []T, p page) {
	out, next := slicePage(items, p)
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// ---- error mapping ---------------------------------------------------------------------------

type macOwnerDetails struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	Flat     string `json:"flat"`
	SiteID   string `json:"site_id"`
	SiteName string `json:"site_name"`
}

// billingErr turns an error from the billing service into the envelope. mac is
// the (normalised) MAC of the request, for mac_in_use.
func (s *Server) billingErr(w http.ResponseWriter, err error, mac string) {
	var dup *billing.DuplicateMACError
	var blocked *billing.RouterBlockedError
	var fe *billing.FieldError
	switch {
	case errors.As(err, &dup) && dup.OtherBuilding:
		apiMACOtherBuilding(w, dup, mac)
	case errors.As(err, &fe):
		validationErr(w, map[string]string{fe.Field: fe.Msg})
	case errors.Is(err, store.ErrDuplicateUnit):
		apiErr(w, http.StatusConflict, codeUnitInUse, err.Error(), nil)
	case errors.Is(err, billing.ErrApartmentExists), errors.Is(err, store.ErrDuplicateApartment):
		apiErr(w, http.StatusConflict, codeApartmentNameUsed, "An apartment with that name already exists.", nil)
	case errors.As(err, &blocked):
		apiErr(w, http.StatusConflict, codeRouterServes, err.Error(), map[string]any{"router": blocked.Router, "apartment": blocked.Apartment, "units": blocked.Units})
	case errors.Is(err, billing.ErrDeviceChanged):
		apiErr(w, http.StatusConflict, "bad_state", err.Error(), nil)
	case errors.Is(err, billing.ErrNoApartment):
		validationErr(w, map[string]string{"apartment_id": "choose an apartment"})
	case errors.Is(err, billing.ErrChooseRouter):
		validationErr(w, map[string]string{"site_id": "choose a router: this apartment is served by more than one"})
	case errors.Is(err, billing.ErrApartmentNoRoute):
		validationErr(w, map[string]string{"apartment_id": "this apartment has no router yet: link a router to it first"})
	case errors.Is(err, billing.ErrRouterNotLinked):
		validationErr(w, map[string]string{"site_id": "that router does not serve this apartment"})
	case strings.Contains(err.Error(), "still has") && strings.Contains(err.Error(), "account(s)"):
		apiErr(w, http.StatusConflict, codeApartmentInUse, err.Error(), nil)
	case errors.As(err, &dup):
		apiErr(w, http.StatusConflict, codeMACInUse, "That MAC address is already used by house "+dup.Flat+" ("+dup.Code+") in "+dup.SiteName+".",
			map[string]any{"mac": mac, "account": macOwnerDetails{dup.AccountID, dup.Code, dup.Flat, dup.SiteID, dup.SiteName}})
	case errors.Is(err, store.ErrDuplicateMAC):
		apiErr(w, http.StatusConflict, codeMACInUse, "That MAC address is already used by another account.", map[string]any{"mac": mac})
	case errors.Is(err, billing.ErrRouterOwnMAC):
		validationErr(w, map[string]string{"mac": billing.ErrRouterOwnMAC.Error()})
	case errors.Is(err, billing.ErrBadMAC):
		validationErr(w, map[string]string{"mac": "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"})
	case errors.Is(err, billing.ErrNotFound):
		apiErr(w, http.StatusNotFound, codeNotFound, "Not found.", nil)
	case errors.Is(err, billing.ErrPossibleDuplicate):
		apiErr(w, http.StatusConflict, "possible_duplicate", err.Error()+". Send accept_duplicate: true to assign it anyway, or resolve it as ignored.", nil)
	case errors.Is(err, billing.ErrLowerPlan):
		apiErr(w, http.StatusConflict, codeLowerPlan, "The account is active on a faster plan. A slower one waits for it to end (use force to change the speed now).", nil)
	case errors.Is(err, billing.ErrDuplicateKey):
		apiErr(w, http.StatusConflict, codeDupPayment, "That transaction code is already recorded.", nil)
	case errors.Is(err, billing.ErrBadState):
		apiErr(w, http.StatusConflict, codeBadState, "That payment is not waiting for a decision.", nil)
	case errors.Is(err, billing.ErrNoSite):
		validationErr(w, map[string]string{"site_id": "choose a router"})
	case errors.Is(err, billing.ErrBadPhone):
		validationErr(w, map[string]string{"phone": err.Error()})
	case errors.Is(err, billing.ErrBadTransCode):
		validationErr(w, map[string]string{"trans_code": err.Error()})
	case errors.Is(err, billing.ErrNoPackage):
		validationErr(w, map[string]string{"package_id": err.Error()})
	case errors.Is(err, store.ErrUnavailable), errors.Is(err, store.ErrPersist):
		apiErr(w, http.StatusServiceUnavailable, codeDBDown, "The database is unavailable. Try again in a moment.", nil)
	case strings.Contains(err.Error(), "already exists in this building"):
		apiErr(w, http.StatusConflict, codeFlatInUse, err.Error(), nil)
	case strings.Contains(err.Error(), "choose a package"):
		validationErr(w, map[string]string{"package_id": "choose a package"})
	case strings.Contains(err.Error(), "house number is required"), strings.Contains(err.Error(), "the house number is required"):
		validationErr(w, map[string]string{"unit": "a house number is required, e.g. B4"})
	default:
		s.log.Warn("api: unexpected error", "err", err)
		apiErr(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
	}
}

// storeErr maps store failures; it reports whether it wrote an answer.
func (s *Server) storeErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, store.ErrUnavailable) || errors.Is(err, store.ErrPersist) {
		apiErr(w, http.StatusServiceUnavailable, codeDBDown, "The database is unavailable. Try again in a moment.", nil)
		return true
	}
	return false
}

// audit records a state-changing call with who and from where.
func (c *apiCtx) audit(s *Server, msg string) { s.authn.AuditNow(c.user.Username, c.ip, "api: "+msg) }

// ---- routes ----------------------------------------------------------------------------------

func (s *Server) apiRoutes(mux *http.ServeMux) {
	const (
		viewer   = store.RoleViewer
		operator = store.RoleOperator
		owner    = store.RoleOwner
	)
	// Every /api/v1/sites… route is also served at /api/v1/routers… (D8).
	h := func(pattern, role string, f apiFunc) {
		for _, p := range aliasPatterns(pattern) {
			mux.Handle(p, s.api(role, f))
		}
	}
	open := func(pattern string, f http.HandlerFunc) { mux.HandleFunc(pattern, f) }
	// Sign-in and refresh need no token.
	open("POST /api/v1/auth/login", s.apiLogin)
	open("POST /api/v1/auth/refresh", s.apiRefresh)
	h("POST /api/v1/auth/logout", viewer, s.apiLogout)
	h("GET /api/v1/me", viewer, s.apiMe)
	h("GET /api/v1/auth/devices", viewer, s.apiDevices)
	h("DELETE /api/v1/auth/devices/{id}", viewer, s.apiDeviceDelete)

	h("GET /api/v1/dashboard", viewer, s.apiDashboard)
	h("GET /api/v1/sites", viewer, s.apiSites)
	h("GET /api/v1/sites/{id}", viewer, s.apiSite)
	h("GET /api/v1/sites/{id}/qoe", viewer, s.apiQoE)
	h("GET /api/v1/sites/{id}/onts", viewer, s.apiONTs)
	h("GET /api/v1/accounts", viewer, s.apiAccounts)
	h("GET /api/v1/accounts/{id}", viewer, s.apiAccount)
	h("GET /api/v1/packages", viewer, s.apiPackages)
	h("GET /api/v1/payments", viewer, s.apiPayments)
	h("GET /api/v1/alerts", viewer, s.apiAlerts)
	h("GET /api/v1/unregistered", viewer, s.apiUnregistered)
	h("GET /api/v1/search", viewer, s.apiSearch)
	h("GET /api/v1/audit", owner, s.apiAudit)

	h("POST /api/v1/accounts", operator, s.apiAccountCreate)
	h("PATCH /api/v1/accounts/{id}", operator, s.apiAccountPatch)
	h("PUT /api/v1/accounts/{id}/mac", operator, s.apiAccountMAC)
	h("POST /api/v1/accounts/{id}/suspend", operator, s.apiAccountSuspend(true))
	h("POST /api/v1/accounts/{id}/unsuspend", operator, s.apiAccountSuspend(false))
	h("POST /api/v1/accounts/{id}/package", operator, s.apiAccountPackage)
	h("POST /api/v1/accounts/{id}/extend", operator, s.apiAccountExtend)
	h("POST /api/v1/accounts/{id}/reconcile", operator, s.apiAccountReconcile)
	h("POST /api/v1/payments", operator, s.apiPaymentCreate)
	h("POST /api/v1/payments/{key}/assign", operator, s.apiPaymentAssign)
	h("POST /api/v1/payments/{key}/resolve", operator, s.apiPaymentResolve)
	h("POST /api/v1/alerts/mute", operator, s.apiAlertMute)

	h("POST /api/v1/sites/{id}/unregistered/refresh", operator, s.apiUnregRefresh)
	h("POST /api/v1/unregistered/attach", operator, s.apiUnregAttach)
	h("POST /api/v1/unregistered/create", operator, s.apiUnregCreate)
	h("POST /api/v1/unregistered/dismiss", operator, s.apiUnregDismiss)
	h("POST /api/v1/unregistered/move", operator, s.apiUnregMove)

	h("GET /api/v1/apartments", viewer, s.apiApartments)
	h("GET /api/v1/apartments/{id}", viewer, s.apiApartment)
	h("POST /api/v1/apartments", operator, s.apiApartmentCreate)
	h("PATCH /api/v1/apartments/{id}", operator, s.apiApartmentPatch)
	h("DELETE /api/v1/apartments/{id}", owner, s.apiApartmentDelete)
	h("POST /api/v1/apartments/{id}/routers", operator, s.apiApartmentLink)
	h("DELETE /api/v1/apartments/{id}/routers/{site_id}", operator, s.apiApartmentUnlink)
	h("POST /api/v1/accounts/{id}/move", operator, s.apiAccountMove)
	// The router contacts were replaced by the apartment's caretaker and landlord.
	h("GET /api/v1/sites/{id}/contacts", viewer, apiGone)
	h("POST /api/v1/sites/{id}/contacts", operator, apiGone)
	h("PATCH /api/v1/sites/{id}/contacts/{cid}", operator, apiGone)
	h("DELETE /api/v1/sites/{id}/contacts/{cid}", operator, apiGone)

	// One hook per later stream; each adds its routes in its own file.
	s.setupRoutes(h)
	s.liveRoutes(h)
	s.portsRoutes(h)
	s.overviewRoutes(h)

	// Anything else under /api/v1/ is a clean JSON 404 (and cookies are never read here).
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such call.", nil)
	})
}
