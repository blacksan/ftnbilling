package acs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ispbill/internal/store"
)

// Showing the current Wi-Fi password of a house.
//
// The server keeps the password it set (sealed with DATA_KEY, Account.HomeWiFi). Showing it needs no ACS session:
// the saved one is opened and returned. When the ONU is online the ACS also asks the box for the passphrase of each
// radio (GetParameterValues on the WLAN KeyPassphrase / PreSharedKey) in its next session and says what the box
// returned; a box that returns nothing (many hide it) or a blank leaves the saved one in place. Every view writes an
// audit entry first. Only the customer's Wi-Fi key is shown here; system logins keep their own masking and step-up.

// WiFiKey is the password of one network of the house.
type WiFiKey struct {
	Band     string `json:"band"`
	SSID     string `json:"ssid"`
	Password string `json:"password"`
	Source   string `json:"source"`            // "onu" (the box returned it) | "saved" (the one this server set) | "none"
	Differs  bool   `json:"differs,omitempty"` // the box returned another one than the saved one
}

// WiFiReveal is the answer of RevealWiFi.
type WiFiReveal struct {
	Networks []WiFiKey `json:"networks"`
	Asked    bool      `json:"asked_onu"` // the box was asked
	Note     string    `json:"note,omitempty"`
}

// liveRead is one pending "read the passphrases from the box" request.
type liveRead struct {
	done chan struct{}
	once sync.Once
	pass map[string]string // band -> passphrase the box returned (blank ones left out)
	err  string
}

// finish ends the wait (once).
func (r *liveRead) finish() { r.once.Do(func() { close(r.done) }) }

type liveReads struct {
	mu sync.Mutex
	m  map[string]*liveRead
}

// Has5GHz says whether the ONU has a 5 GHz radio, from the Wi-Fi networks it reported.
func Has5GHz(o store.ONU) bool {
	for _, w := range o.WiFi {
		if w.Band == "5" {
			return true
		}
	}
	return false
}

// bandsOf lists the bands the ONU reported ("2.4", "5"), in that order.
func bandsOf(o store.ONU) []string {
	var out []string
	for _, b := range []string{"2.4", "5"} {
		for _, w := range o.WiFi {
			if w.Band == b {
				out = append(out, b)
				break
			}
		}
	}
	return out
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// serveLiveRead answers a pending request for this ONU: it reads the passphrase of the main network of each band.
func (s *Service) serveLiveRead(ctx context.Context, ss *session, onuID string) {
	s.live.mu.Lock()
	req := s.live.m[onuID]
	s.live.mu.Unlock()
	if req == nil {
		return
	}
	defer req.finish()
	o, ok := s.loadONU(onuID)
	if !ok {
		req.err = "the ONU is gone"
		return
	}
	prof := pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass)
	leaves := append([]string(nil), prof.Pass...)
	if o.Root != store.RootTR181 {
		leaves = append(leaves, "PreSharedKey.1.PreSharedKey")
	}
	got := map[string]string{}
	for _, band := range []string{"2.4", "5"} {
		w, ok := primary(o.WiFi, band)
		if !ok {
			continue
		}
		for _, leaf := range leaves {
			name := apPath(w) + leaf
			vals, f, err := ss.gpv(ctx, []string{name})
			if err != nil {
				req.err = "the ONU stopped answering"
				return
			}
			if f != nil {
				continue
			}
			v := valueOf(vals, name)
			if v == "" || strings.Trim(v, "*") == "" || isHex64(v) || ValidPassphrase(v) != "" {
				continue // blank, masked, or the raw 64-digit key: not a passphrase a person can type
			}
			got[band] = v
			break
		}
	}
	req.pass = got
}

// RevealWiFi writes the audit entry, then returns the password of each network of the house: the one the box
// returned when it could be asked, else the saved one.
func (s *Service) RevealWiFi(ctx context.Context, by, ip, accountID string) (WiFiReveal, error) {
	var res WiFiReveal
	var o store.ONU
	var want store.WiFiDesired
	var house string
	err := s.st.Update(func(d *store.Data) error {
		var ok bool
		if o, ok = d.ONUOfAccount(accountID); !ok {
			return ErrNoONU
		}
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNoAccount
		}
		want, house = a.HomeWiFi, a.Code
		// the password is not in the entry
		s.audit(d, o, by, ip, fmt.Sprintf("admin %s viewed the Wi-Fi password for house %s", by, house))
		return nil
	})
	if err != nil {
		return res, err
	}
	var saved, saved5 string
	if want.Set {
		var err error
		if saved, err = s.openPass(want.PassEnc); err != nil {
			return res, errors.New("the saved Wi-Fi password cannot be opened (DATA_KEY changed?)")
		}
		saved5 = saved
		if !want.Same && want.Pass5Enc != "" {
			if saved5, err = s.openPass(want.Pass5Enc); err != nil {
				return res, errors.New("the saved 5 GHz Wi-Fi password cannot be opened")
			}
		}
	}
	live := map[string]string{}
	if s.online(o) && o.Root != "" && len(o.WiFi) > 0 {
		res.Asked = true
		req := &liveRead{done: make(chan struct{})}
		s.live.mu.Lock()
		if s.live.m == nil {
			s.live.m = map[string]*liveRead{}
		}
		s.live.m[o.ID] = req
		s.live.mu.Unlock()
		s.wg.Add(1)
		go func() { // ask the box to connect now; when it cannot be reached there is no point in waiting
			defer s.wg.Done()
			_ = s.ConnectionRequest(context.Background(), o.ID)
			if k, ok := s.kickOf(o.ID); ok && !k.OK {
				req.err = "the ONU did not answer the request"
				req.finish()
			}
		}()
		select {
		case <-req.done:
		case <-time.After(liveReadWait):
		case <-ctx.Done():
		}
		s.live.mu.Lock()
		delete(s.live.m, o.ID)
		s.live.mu.Unlock()
		select {
		case <-req.done:
			live = req.pass
			if req.err != "" {
				res.Note = "The ONU did not answer. Showing the saved password."
			}
		default:
			res.Note = "The ONU did not answer in time. Showing the saved password."
		}
	}
	// the main network of each band
	for _, band := range []string{"2.4", "5"} {
		w, ok := primary(o.WiFi, band)
		if !ok {
			continue
		}
		k := WiFiKey{Band: band, SSID: w.SSID, Password: saved, Source: "none"}
		if band == "5" {
			k.Password = saved5
		}
		if k.Password != "" {
			k.Source = "saved"
		}
		if lp := live[band]; lp != "" {
			k.Differs = k.Password != "" && lp != k.Password
			k.Password, k.Source = lp, "onu"
		}
		res.Networks = append(res.Networks, k)
	}
	if len(res.Networks) == 0 { // the box has not reported its networks: the saved one
		k := WiFiKey{SSID: want.SSID, Password: saved, Source: "none"}
		if saved != "" {
			k.Source = "saved"
		}
		res.Networks = append(res.Networks, k)
	}
	if res.Note == "" && res.Asked && len(live) == 0 {
		res.Note = "The ONU did not return its password (many do not). Showing the saved one."
	}
	for _, k := range res.Networks {
		if k.Differs {
			res.Note = "The password on the ONU is not the one saved here. The tenant may have changed it on the box."
		}
	}
	if len(res.Networks) == 1 && res.Networks[0].Source == "none" {
		res.Note = "No Wi-Fi password is saved for this house, and the ONU did not return one."
	}
	return res, nil
}

// liveReadWait is how long RevealWiFi waits for the ONU's session.
const liveReadWait = 12 * time.Second

// online says whether the ONU checked in recently enough to answer a connection request.
func (s *Service) online(o store.ONU) bool {
	iv := s.informEvery()
	return !o.LastInform.IsZero() && s.now().Sub(o.LastInform) < 3*iv
}
