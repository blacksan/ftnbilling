package acs

import (
	"encoding/json"
	"strings"
	"time"

	"ispbill/internal/store"
)

// The per-model "what worked" table. A passphrase name that a box ACCEPTS is not proof that the Wi-Fi changed (one
// ONU answered success for a vendor name and kept its old password). So the name that worked is recorded only when the
// owner confirms ("It works") that a phone joined the new network; from then on every box of that model (OUI, product
// class and firmware) tries that name first. The table lives in one setting (JSON) and holds no secret.

const keyWorks = "acs.works"

// Work is one confirmed entry.
type Work struct {
	OUI       string    `json:"oui"`
	Class     string    `json:"class"`
	Firmware  string    `json:"firmware,omitempty"`
	Profile   string    `json:"profile"`
	PassLeaf  string    `json:"pass_leaf"`
	Count     int       `json:"count"` // confirmations
	Confirmed time.Time `json:"confirmed"`
	By        string    `json:"by,omitempty"`
}

func modelKey(oui, class, fw string) string {
	return strings.ToUpper(strings.TrimSpace(oui)) + "|" + strings.TrimSpace(class) + "|" + strings.TrimSpace(fw)
}

func readWorks(d *store.Data) map[string]Work {
	m := map[string]Work{}
	if v := d.Settings[keyWorks].Value; v != "" {
		_ = json.Unmarshal([]byte(v), &m)
	}
	return m
}

func writeWorks(d *store.Data, m map[string]Work, now time.Time) {
	b, _ := json.Marshal(m)
	d.Settings[keyWorks] = store.Setting{Key: keyWorks, Value: string(b), UpdatedAt: now}
}

// workFor finds the confirmed name for a model: the exact firmware first, else any firmware of the same OUI and class.
func workFor(m map[string]Work, o store.ONU) (Work, bool) {
	if w, ok := m[modelKey(o.OUI, o.ProductClass, o.SWVersion)]; ok {
		return w, true
	}
	var best Work
	found := false
	for _, w := range m {
		if strings.EqualFold(w.OUI, o.OUI) && w.Class == o.ProductClass && (!found || w.Confirmed.After(best.Confirmed)) {
			best, found = w, true
		}
	}
	return best, found
}

// passCandidates is the order in which passphrase names are tried on a box: the one the owner confirmed on this
// box, the one confirmed for its model, then the profile's order. Names the owner said did not work come last.
func passCandidates(d *store.Data, o store.ONU, prof Profile) []string {
	var first []string
	if o.Profile.PassLeaf != "" {
		first = append(first, o.Profile.PassLeaf)
	}
	if w, ok := workFor(readWorks(d), o); ok {
		first = append(first, w.PassLeaf)
	}
	bad := map[string]bool{}
	for _, b := range o.Profile.BadLeaves {
		bad[b] = true
	}
	var out, last []string
	seen := map[string]bool{}
	for _, l := range append(first, prof.Pass...) {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		// A name the owner said did not work on this box goes last, even when the model table puts it first (else "It
		// didn't work" would send the same name again, for ever). Only the name confirmed on this very box is exempt.
		if bad[l] && l != o.Profile.PassLeaf {
			last = append(last, l)
		} else {
			out = append(out, l)
		}
	}
	return append(out, last...)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// WorkView is a table entry for the owner's screen.
type WorkView struct {
	OUI       string    `json:"oui"`
	Class     string    `json:"product_class"`
	Firmware  string    `json:"firmware,omitempty"`
	PassLeaf  string    `json:"pass_leaf"`
	Count     int       `json:"confirmations"`
	Confirmed time.Time `json:"confirmed_at"`
}

// Works lists the confirmed entries.
func (s *Service) Works() []WorkView {
	var out []WorkView
	s.st.View(func(d *store.Data) {
		for _, w := range readWorks(d) {
			out = append(out, WorkView{OUI: w.OUI, Class: w.Class, Firmware: w.Firmware, PassLeaf: w.PassLeaf, Count: w.Count, Confirmed: w.Confirmed})
		}
	})
	return out
}
