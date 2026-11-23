package store

import (
	"sort"
	"strconv"
	"strings"
)

// The equipment hierarchy: router (Site) > OLT > PON port > ONU > house. These are read helpers over Data; the
// rules that change it live in billing (olt.go).

// OLTLabel is how an OLT is named on a page: its name, else model and serial.
func OLTLabel(e Equipment) string {
	switch {
	case strings.TrimSpace(e.Name) != "":
		return e.Name
	case e.Model != "" && e.Serial != "":
		return e.Model + " " + e.Serial
	case e.Model != "":
		return e.Model
	case e.Serial != "":
		return e.Serial
	}
	return "OLT"
}

// OLTByMAC finds the live (not retired) OLT that has the MAC.
func (d *Data) OLTByMAC(mac string) (Equipment, bool) {
	for _, e := range d.Equipment {
		if e.Kind == KindOLT && e.Status != EqRetired && e.MAC != "" && strings.EqualFold(e.MAC, mac) {
			return e, true
		}
	}
	return Equipment{}, false
}

// OLTsOfSite lists the OLTs plugged into a router, by name.
func (d *Data) OLTsOfSite(siteID string) []Equipment {
	var out []Equipment
	for _, e := range d.Equipment {
		if e.Kind == KindOLT && e.Status != EqRetired && e.SiteID == siteID && siteID != "" {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(OLTLabel(out[i])), strings.ToLower(OLTLabel(out[j]))
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// UnplacedOLTs lists the live OLTs that are not on a router (in stock, returned).
func (d *Data) UnplacedOLTs() []Equipment {
	var out []Equipment
	for _, e := range d.Equipment {
		if e.Kind == KindOLT && e.Status != EqRetired && (e.SiteID == "" || d.Sites[e.SiteID].Removed) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(OLTLabel(out[i])) < strings.ToLower(OLTLabel(out[j])) })
	return out
}

// ONUsOfOLT lists the ONUs placed under an OLT, by PON port then serial. port 0 = every port.
func (d *Data) ONUsOfOLT(oltID string, port int) []Equipment {
	var out []Equipment
	for _, e := range d.Equipment {
		if e.Kind == KindONT && e.OLTID == oltID && oltID != "" && e.Status != EqRetired && (port == 0 || e.PONPort == port) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PONPort != out[j].PONPort {
			return out[i].PONPort < out[j].PONPort
		}
		if out[i].Serial != out[j].Serial {
			return out[i].Serial < out[j].Serial
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// UnplacedONUs lists the live ONTs that name no OLT.
func (d *Data) UnplacedONUs() []Equipment {
	var out []Equipment
	for _, e := range d.Equipment {
		if e.Kind != KindONT || e.Status == EqRetired {
			continue
		}
		if o, ok := d.Equipment[e.OLTID]; e.OLTID == "" || !ok || o.Kind != KindOLT || o.Status == EqRetired { // a retired OLT is not in the tree
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Serial != out[j].Serial {
			return out[i].Serial < out[j].Serial
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ONTByMAC finds the live ONT of the register whose WAN MAC is mac.
func (d *Data) ONTByMAC(mac string) (Equipment, bool) {
	var best Equipment
	found := false
	for _, e := range d.Equipment {
		if e.Kind == KindONT && e.Status != EqRetired && e.MAC != "" && strings.EqualFold(e.MAC, mac) {
			// An in-stock one wins over an installed one (it is the one that can be given to a house).
			if !found || (e.Status == EqInStock && best.Status != EqInStock) {
				best, found = e, true
			}
		}
	}
	return best, found
}

// PONPortNumbers lists the PON ports of an OLT: 1..PONPorts, plus any port that has a mapping or an ONU.
func (d *Data) PONPortNumbers(olt Equipment) []int {
	set := map[int]bool{}
	for i := 1; i <= olt.PONPorts; i++ {
		set[i] = true
	}
	for k := range olt.PONApartments {
		if n := atoiPort(k); n > 0 {
			set[n] = true
		}
	}
	for _, o := range d.ONUsOfOLT(olt.ID, 0) {
		if o.PONPort > 0 {
			set[o.PONPort] = true
		}
	}
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func atoiPort(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 1000 {
		return 0
	}
	return n
}

// PONKey is the key of Equipment.PONApartments for a port number.
func PONKey(port int) string {
	if port <= 0 {
		return ""
	}
	return strconv.Itoa(port)
}

// PONApartmentIDs lists the apartments a PON port serves.
func (e Equipment) PONApartmentIDs(port int) []string { return e.PONApartments[PONKey(port)] }

// PONServes reports whether the OLT's PON port serves the apartment. With port 0 it asks every port. known is false
// when the OLT has no mapping at all for what was asked (then nothing can be said and no warning is due).
func (e Equipment) PONServes(port int, apartmentID string) (serves, known bool) {
	if port > 0 {
		ids := e.PONApartmentIDs(port)
		if len(ids) == 0 {
			return false, false
		}
		for _, id := range ids {
			if id == apartmentID {
				return true, true
			}
		}
		return false, true
	}
	any := false
	for _, ids := range e.PONApartments {
		for _, id := range ids {
			any = true
			if id == apartmentID {
				return true, true
			}
		}
	}
	return false, any
}
