package olt

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"ispbill/internal/store"
)

// "Secure this OLT": the server replaces the OLT's login and enable password with generated ones, tests them with a new
// SSH session while the old session is still open, saves the configuration only then, stores the new passwords sealed,
// removes the default SNMP communities and (once its syntax is known) restricts SSH to the router.
//
// Every user, password and SNMP line of this action goes through Session.RunSecure, whose exact shapes are the only
// exception to the driver's denylist. `sshd disable` is never sent.

// Generated passwords: letters and digits only (the CLI reads '?' as help, '!' as a comment and a space as the end of the
// word), at least 24 long.
const (
	secretLen      = 28
	secretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// GenPassword makes a random password of letters and digits with at least one lower case letter, one upper case letter and
// one digit.
func GenPassword() (string, error) {
	for tries := 0; tries < 50; tries++ {
		b := make([]byte, secretLen)
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(secretAlphabet))))
			if err != nil {
				return "", err
			}
			b[i] = secretAlphabet[n.Int64()]
		}
		p := string(b)
		if strings.ContainsAny(p, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(p, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(p, "0123456789") {
			return p, nil
		}
	}
	return "", errors.New("could not generate a password")
}

// SecureResult says what the action did. It holds no secret.
type SecureResult struct {
	EnableChecked bool     // the enable password was exercised by the verification session
	SNMPRemoved   []string // default communities removed
	SNMPNote      string
	ACLNote       string
	ACLDone       bool     // SSH is restricted to the router's address, verified with a new session, saved
	ACLRules      []string // the login-access-list rules the OLT showed afterwards (read back)
	ACLFinding    string   // what is still open (web)
}

// Secure replaces an OLT's login and enable password with generated ones (the owner's action; step-up is the caller's).
func (s *Service) Secure(ctx context.Context, id, by string) (SecureResult, error) {
	var res SecureResult
	_, st, ok := s.Load(id)
	if !ok {
		return res, ErrNotFound
	}
	if st.Settings.Adopt.SerialOK != "ok" {
		return res, ErrNotIdentified
	}
	cands := s.credCandidates(id, true)
	if len(cands) == 0 {
		return res, ErrNotConfigured
	}
	var lastErr error
	for _, c := range cands {
		c := c
		err := s.withSessionAs(ctx, id, true, &c, func(o *opened) error {
			var err error
			res, err = s.secureIn(ctx, o, c, by)
			return err
		})
		if errors.Is(err, ErrLoginFailed) {
			lastErr = err
			continue
		}
		return res, err
	}
	return res, lastErr
}

// secureIn is the action inside the session that is logged in with the old login (cur); that session stays open until it
// returns.
func (s *Service) secureIn(ctx context.Context, o *opened, cur cred, by string) (SecureResult, error) {
	var res SecureResult
	dl := s.o.Dialect
	e := o.e
	label := store.OLTLabel(e)
	if s.o.Box == nil {
		return res, ErrNoKey
	}
	// Still the same device? (An OLT could have been swapped since the last check.)
	out, err := o.sess.ShowAt(ctx, dl.VersionMode, dl.VersionCmd)
	if err != nil {
		return res, err
	}
	vi := ParseVersion(out)
	if ok, why := IdentityOK(e, vi); !ok {
		s.markAdopt(e.ID, store.AdoptMismatch, "The device on the port is not this OLT ("+why+"). Nothing was changed.", "")
		return res, ErrNotIdentified
	}
	// A saved copy of the configuration first (a failure to read it does not stop the change: the old passwords are known).
	if _, _, err := s.backupIn(ctx, o, "before Secure this OLT", by); err != nil {
		s.o.Log.Warn("backup before securing an OLT failed", "olt", e.ID, "err", errText(err))
	}
	login, err := GenPassword()
	if err != nil {
		return res, err
	}
	enable, err := GenPassword()
	if err != nil {
		return res, err
	}
	user := cur.User
	if !userRE.MatchString(user) {
		return res, errors.New("the OLT's login name cannot be used here")
	}
	// The new pair is stored sealed BEFORE the OLT changes: if the server stops half-way, the pair is not lost.
	sl, err1 := s.o.Box.SealAAD(login, mgmtAAD)
	se, err2 := s.o.Box.SealAAD(enable, enableAAD)
	if err1 != nil || err2 != nil {
		return res, errors.New("could not seal the new passwords")
	}
	if err := s.setAdopt(e.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) { a.PendingPass, a.PendingEn = sl, se }); err != nil {
		return res, err
	}
	clearPending := func() {
		_ = s.setAdopt(e.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) { a.PendingPass, a.PendingEn = "", "" })
	}
	// enableSent: the generated enable password may be on the OLT now. False only when the change stopped at a line the
	// OLT refused before or at the enable line (user manager, the login line, the enable line itself).
	enableSent := true
	restore := func(why string) error {
		// The old session is still open: put the old passwords back through it.
		rerr := o.sess.RunRestore(ctx, dl.SecureUserLines(user, cur.Pass, cur.Enable))
		if rerr != nil {
			// The OLT may now hold the new pair (the old session broke, or the OLT refused the old line): the pair stays
			// sealed as pending, and the next pass logs in with it when the stored login no longer works.
			return fmt.Errorf("%s; the old password could NOT be restored automatically (%s): the new passwords are kept sealed and the server tries them on its next check", why, errText(rerr))
		}
		if cur.Enable == "" && enableSent {
			// There was no known enable password to put back: the OLT keeps the generated one (until it restarts
			// without a save), so the server keeps it too rather than leave the OLT with an enable password nobody knows.
			_ = s.upd(e.ID, func(d *store.Data, eq store.Equipment, st *store.OLTState) {
				st.Settings.EnablePass = se
				st.Settings.Adopt.PendingPass, st.Settings.Adopt.PendingEn = "", ""
				s.audit(d, eq, firstNonEmpty(by, "system"), "OLT "+label+": the old login password was restored; there was no known enable password to restore, so the generated one is now stored")
			})
			return fmt.Errorf("%s; the old login password was restored and nothing was saved (the OLT had no known enable password: the generated one is stored and can be revealed)", why)
		}
		clearPending()
		return fmt.Errorf("%s; the old password was restored and nothing was saved", why)
	}
	if err := o.sess.RunSecure(ctx, dl.SecureUserLines(user, login, enable)); err != nil {
		var ce *CmdError
		if errors.As(err, &ce) && ce.Cmd != dl.ExitCmd {
			enableSent = false
		}
		return res, s.secureFailed(e, by, restore("the OLT refused the new password ("+errText(err)+")"))
	}
	// Verify with a NEW session: the login, the shown serial and, where the session starts at the user prompt, the enable password.
	checked, verr := s.verifyNew(ctx, o, user, login, enable, vi.Serial)
	if verr != nil {
		return res, s.secureFailed(e, by, restore("the new password did not work in a new session ("+errText(verr)+")"))
	}
	res.EnableChecked = checked
	// Only now is the change saved on the OLT.
	if err := o.sess.Save(ctx); err != nil {
		return res, s.secureFailed(e, by, restore("the OLT would not save its configuration ("+errText(err)+")"))
	}
	// And stored: the sealed login replaces the old one.
	err = s.upd(e.ID, func(d *store.Data, eq store.Equipment, st *store.OLTState) {
		eq.MgmtUser, eq.MgmtPass = user, sl
		if eq.MgmtIP == "" {
			eq.MgmtIP = factoryIP
		}
		d.Equipment[e.ID] = eq
		st.Settings.EnablePass = se
		a := &st.Settings.Adopt
		a.PendingPass, a.PendingEn = "", ""
		a.Secured, a.SecuredAt, a.SecuredBy, a.CredSource, a.Adopted = true, s.now(), by, "secured", true
		a.Stage, a.Note, a.Error, a.CheckedAt = store.AdoptSecured, "Adopted and secured.", "", s.now()
		note := "login and enable password replaced by generated ones, tested with a new session"
		if !checked {
			note += " (the enable password could not be tested: SSH starts privileged)"
		}
		s.audit(d, eq, firstNonEmpty(by, "system"), "OLT "+label+" secured: "+note+", configuration saved")
	})
	if err != nil {
		return res, fmt.Errorf("the OLT now uses the new passwords but the server could not store them (they are kept sealed as pending): %w", err)
	}
	// SNMP: remove the default communities.
	s.secureSNMP(ctx, o, by, &res)
	// SSH access list: restrict SSH to the router's OLT-management address (verified with a new session before it is saved).
	s.secureACL(ctx, o, user, login, enable, vi.Serial, by, &res)
	_ = s.setAdopt(e.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
		a.SNMPNote, a.SNMPOff = res.SNMPNote, len(res.SNMPRemoved) > 0 || strings.Contains(res.SNMPNote, "SNMP is off")
		a.ACLNote, a.ACLDone, a.ACLRules, a.ACLFinding = res.ACLNote, res.ACLDone, res.ACLRules, res.ACLFinding
	})
	return res, nil
}

func (s *Service) secureFailed(e store.Equipment, by string, err error) error {
	_ = s.upd(e.ID, func(d *store.Data, eq store.Equipment, st *store.OLTState) {
		st.Settings.Adopt.Error = errText(err)
		s.audit(d, eq, firstNonEmpty(by, "system"), "OLT "+store.OLTLabel(eq)+": Secure this OLT failed: "+errText(err))
	})
	return err
}

var userRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

// verifyNew opens a new SSH session with the new login and checks that it reaches the same device (same serial) and, where
// the session starts at the user prompt, that the enable password works. The old session is not touched.
func (s *Service) verifyNew(ctx context.Context, o *opened, user, login, enable, serial string) (bool, error) {
	pin := o.st.Settings.HostKey
	if pin == "" {
		pin = o.sess.NewHostKey
	}
	sess, err := Open(ctx, s.o.Dialect, Target{Addr: o.addr, Proto: store.OLTProtoSSH, User: user, Pass: login, Enable: enable, PinnedKey: pin,
		Protect: o.st.Settings.MgmtVLANOrDefault()}, s.o.Dial)
	if err != nil {
		return false, err
	}
	defer sess.Close()
	sess.Timeout = s.o.Timeout
	checked, err := sess.VerifyEnable(ctx)
	if err != nil {
		return checked, err
	}
	out, err := sess.ShowAt(ctx, s.o.Dialect.VersionMode, s.o.Dialect.VersionCmd)
	if err != nil {
		return checked, err
	}
	if NormSerial(ParseVersion(out).Serial) != NormSerial(serial) {
		return checked, errors.New("the new session reached a device with another serial number")
	}
	return checked, nil
}

// secureSNMP removes the default SNMP communities (public, private) and notes that SNMP is now off. The server itself
// never uses SNMP. A failure is noted; it never undoes the password change.
func (s *Service) secureSNMP(ctx context.Context, o *opened, by string, res *SecureResult) {
	dl := s.o.Dialect
	out, err := o.sess.ShowAt(ctx, ModeConfig, ShowSNMPCmd)
	var present, other []string
	if err == nil {
		present, other = defaultCommunities(out)
	} else {
		present = []string{"public", "private"} // could not read the list: try both, a refusal for a missing one is fine
	}
	if len(present) > 0 {
		if err := o.sess.RunSecure(ctx, dl.SecureSNMPLines(present)); err != nil {
			// One missing community must not stop the other: try them one by one.
			var done []string
			for _, w := range present {
				if o.sess.RunSecure(ctx, dl.SecureSNMPLines([]string{w})) == nil {
					done = append(done, w)
				}
			}
			present = done
		}
	}
	// What is left?
	left, other2 := []string{}, []string(nil)
	if out2, err := o.sess.ShowAt(ctx, ModeConfig, ShowSNMPCmd); err == nil {
		left, other2 = defaultCommunities(out2)
		other = other2
	} else {
		res.SNMPNote = "The removal of the default SNMP communities was sent but the list could not be read again to confirm it: check `show snmp-server community` on the OLT."
		return
	}
	if len(left) > 0 {
		res.SNMPNote = "The default SNMP communities " + strings.Join(left, ", ") + " could not be removed. Remove them on the OLT (no snmp-server community WORD)."
		s.auditNote(o.e.ID, by, "OLT "+store.OLTLabel(o.e)+": default SNMP communities could not be removed")
		return
	}
	res.SNMPRemoved = present
	if err := o.sess.Save(ctx); err != nil {
		res.SNMPNote = "The default SNMP communities were removed but the OLT would not save: " + errText(err)
		return
	}
	if len(other) > 0 {
		res.SNMPNote = fmt.Sprintf("The default SNMP communities public and private were removed. %d other community(ies) are still configured; the server did not touch them. The server does not use SNMP.", len(other))
	} else {
		res.SNMPNote = "SNMP is off: the default communities public and private were removed and no community is left. The server does not use SNMP."
	}
	s.auditNote(o.e.ID, by, "OLT "+store.OLTLabel(o.e)+": default SNMP communities removed")
}

// SNMPCommunity is one row of `show snmp-server community`.
type SNMPCommunity struct{ Name, Access string }

// ParseSNMPCommunities reads the table the OLT prints (bench, V1.0.5):
//
//	community               access
//	 public                    ro
//	 private                   rw
//
// The header is skipped; the first column is the name and the second the access (ro or rw). Error lines ("% ...") and
// blank lines are skipped. An empty table gives no rows.
func ParseSNMPCommunities(out string) []SNMPCommunity {
	var rows []SNMPCommunity
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || strings.HasPrefix(f[0], "%") {
			continue
		}
		if strings.EqualFold(f[0], "community") && strings.EqualFold(f[1], "access") {
			continue
		}
		acc := strings.ToLower(f[1])
		if acc != "ro" && acc != "rw" {
			continue
		}
		rows = append(rows, SNMPCommunity{Name: f[0], Access: acc})
	}
	return rows
}

// defaultCommunities lists the default communities (public, private) printed by `show snmp-server community`, and the
// other community names. A line of another layout that still names public or private counts as that community (it must
// never be hidden by a layout the parser does not know).
func defaultCommunities(out string) (def, other []string) {
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		for _, t := range strings.Fields(l) {
			t = strings.Trim(t, `:,;"'`)
			if (t == "public" || t == "private") && !seen[t] {
				seen[t] = true
				def = append(def, t)
			}
		}
	}
	for _, c := range ParseSNMPCommunities(out) {
		if !seen[c.Name] {
			seen[c.Name] = true
			other = append(other, c.Name)
		}
	}
	return def, other
}

// adoptPending makes the pending login (the one an interrupted change was setting) the stored one: it worked just now.
func (s *Service) adoptPending(id, by string) error {
	return s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		a := &st.Settings.Adopt
		if a.PendingPass == "" {
			return
		}
		e.MgmtPass = a.PendingPass
		if a.PendingEn != "" {
			st.Settings.EnablePass = a.PendingEn
		}
		d.Equipment[id] = e
		a.PendingPass, a.PendingEn = "", ""
		a.Secured, a.SecuredAt, a.SecuredBy, a.CredSource, a.Adopted = true, s.now(), firstNonEmpty(by, "system"), "secured", true
		a.Stage, a.Note = store.AdoptSecured, "Adopted and secured (an interrupted change of the login was completed)."
		s.audit(d, e, firstNonEmpty(by, "system"), "OLT "+store.OLTLabel(e)+": the pending generated login became the stored login")
	})
}

func (s *Service) auditNote(id, by, msg string) {
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		s.audit(d, e, firstNonEmpty(by, "system"), msg)
	})
}

// ---- credentials the owner types, and reveal ------------------------------------------------------------------------

// SetCredentials stores the login the owner typed (sealed). The enable password may be empty (none). It does not change
// anything on the OLT.
func (s *Service) SetCredentials(id, user, pass, enable, by string) error {
	if s.o.Box == nil {
		return ErrNoKey
	}
	user = strings.TrimSpace(user)
	if !userRE.MatchString(user) {
		return errors.New("the login name is letters, digits, dash and underscore, starting with a letter")
	}
	if pass == "" || len(pass) > 128 || len(enable) > 128 {
		return errors.New("type the OLT's password (at most 128 characters)")
	}
	for _, v := range []string{pass, enable} {
		for _, c := range v {
			if c < 0x20 || c == 0x7f {
				return errors.New("a password cannot hold a line break or control character")
			}
		}
	}
	sp, err := s.o.Box.SealAAD(pass, mgmtAAD)
	if err != nil {
		return errors.New("could not store the password")
	}
	se := ""
	if enable != "" {
		if se, err = s.o.Box.SealAAD(enable, enableAAD); err != nil {
			return errors.New("could not store the enable password")
		}
	}
	return s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		e.MgmtUser, e.MgmtPass = user, sp
		d.Equipment[id] = e
		st.Settings.EnablePass = se
		a := &st.Settings.Adopt
		a.CredSource, a.Adopted, a.Secured = "owner", false, false
		a.PendingPass, a.PendingEn = "", ""
		if a.Stage == store.AdoptNeedLogin {
			a.Stage, a.Note = "", "Login saved. Press Check now."
		}
		s.audit(d, e, by, "OLT "+store.OLTLabel(e)+": the login the server uses was typed by "+by)
	})
}

// Reveal returns the stored login name and the login and enable passwords. The caller has checked the owner's password;
// every reveal is audited (never with the secrets).
func (s *Service) Reveal(id, by string) (user, login, enable string, err error) {
	if s.o.Box == nil {
		return "", "", "", ErrNoKey
	}
	var sealed, sealedEn string
	var found bool
	s.o.Store.View(func(d *store.Data) {
		e, ok := d.Equipment[id]
		if !ok || e.Kind != store.KindOLT {
			return
		}
		found = true
		user, sealed, sealedEn = e.MgmtUser, e.MgmtPass, d.OLTStateOf(id).Settings.EnablePass
	})
	if !found {
		return "", "", "", ErrNotFound
	}
	if sealed == "" {
		return "", "", "", ErrNotConfigured
	}
	if login, err = s.o.Box.OpenAAD(sealed, mgmtAAD); err != nil {
		return "", "", "", errors.New("the stored OLT password cannot be opened with this server's data key")
	}
	if sealedEn != "" {
		if enable, err = s.o.Box.OpenAAD(sealedEn, enableAAD); err != nil {
			return "", "", "", errors.New("the stored enable password cannot be opened with this server's data key")
		}
	}
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Settings.Adopt.RevealedAt, st.Settings.Adopt.RevealedBy = s.now(), by
		s.audit(d, e, by, "OLT "+store.OLTLabel(e)+": the stored login and enable password were revealed to "+by)
	})
	return user, login, enable, nil
}

// SetManual switches an OLT between the management port (default) and the console snippet.
func (s *Service) SetManual(id string, manual bool, by string) error {
	return s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Settings.Adopt.Manual = manual
		word := "the OLT management port"
		if manual {
			word = "the console snippet (manual)"
		}
		s.audit(d, e, by, "OLT "+store.OLTLabel(e)+": adoption through "+word)
	})
}
