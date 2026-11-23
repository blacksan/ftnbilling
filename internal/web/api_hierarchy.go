package web

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// The equipment hierarchy over the API (router > OLT > PON port > ONU). The OLT's sealed login password is never
// part of any response: only has_mgmt_pass says whether one is stored. There is no API that reveals it.

func (s *Server) apiHierarchyRoutes(h func(string, string, apiFunc)) {
	const (
		viewer   = store.RoleViewer
		operator = store.RoleOperator
	)
	h("GET /api/v1/equipment/tree", viewer, s.apiEquipmentTree)
	h("GET /api/v1/olts", viewer, s.apiOLTList)
	h("POST /api/v1/olts", operator, s.apiOLTSave)
	h("GET /api/v1/olts/{id}", viewer, s.apiOLTGet)
	h("PUT /api/v1/olts/{id}", operator, s.apiOLTSave)
	h("PUT /api/v1/equipment/{id}/placement", operator, s.apiONUPlacement)
}

// apiEquipment is an equipment item as the API shows it: the register fields, never the sealed password.
type apiEquipment struct {
	store.Equipment
	MgmtPass     string   `json:"mgmt_pass,omitempty"` // shadows the stored field: always empty
	HasMgmtPass  bool     `json:"has_mgmt_pass,omitempty"`
	OLTName      string   `json:"olt_name,omitempty"`
	ApartmentIDs []string `json:"apartment_ids,omitempty"` // an ONU: the apartments its PON port serves
	Warnings     []string `json:"warnings,omitempty"`
}

func (s *Server) equipDTO(e store.Equipment) apiEquipment {
	out := apiEquipment{Equipment: e, HasMgmtPass: e.MgmtPass != ""}
	out.Equipment.MgmtPass = ""
	if e.Kind == store.KindONT {
		p := s.svc.PlacementOf(e.ID)
		out.OLTName, out.ApartmentIDs, out.Warnings = p.OLTName, p.ApartmentIDs, p.Warnings
	}
	return out
}

type apiTreeApt struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type apiTreeONU struct {
	ID         string   `json:"id"`
	Serial     string   `json:"serial"`
	GPONSerial string   `json:"gpon_serial,omitempty"`
	Model      string   `json:"model,omitempty"`
	MAC        string   `json:"mac,omitempty"`
	Status     string   `json:"status"`
	OLTID      string   `json:"olt_id,omitempty"`
	PONPort    int      `json:"pon_port,omitempty"`
	House      any      `json:"house"`
	Online     bool     `json:"online"`
	LastSeen   any      `json:"last_seen"`
	TR069      any      `json:"tr069"`
	Warnings   []string `json:"warnings"`
}

type apiTreePON struct {
	Port       int          `json:"port"`
	Apartments []apiTreeApt `json:"apartments"`
	ONUs       []apiTreeONU `json:"onus"`
}

type apiTreeOLT struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Model        string       `json:"model,omitempty"`
	Serial       string       `json:"serial,omitempty"`
	MAC          string       `json:"mac"`
	Firmware     string       `json:"firmware,omitempty"`
	Status       string       `json:"status"`
	RouterID     string       `json:"router_id,omitempty"`
	RouterPort   string       `json:"router_port,omitempty"`
	PONPorts     int          `json:"pon_ports"`
	MgmtIP       string       `json:"mgmt_ip,omitempty"`
	MgmtUser     string       `json:"mgmt_user,omitempty"`
	HasMgmtPass  bool         `json:"has_mgmt_pass"`
	ONUCount     int          `json:"onu_count"`
	OnlineCount  int          `json:"online_count"`
	WarningCount int          `json:"warning_count"`
	PONs         []apiTreePON `json:"pons"`
}

func treeONU(n billing.TreeONU) apiTreeONU {
	o := apiTreeONU{ID: n.Equip.ID, Serial: n.Equip.Serial, GPONSerial: n.Equip.GPONSerial, Model: n.Equip.Model, MAC: n.Equip.MAC, Status: n.Equip.Status,
		OLTID: n.Equip.OLTID, PONPort: n.Equip.PONPort, Online: n.Online, LastSeen: tp(n.LastSeen), Warnings: n.Warnings}
	if o.Warnings == nil {
		o.Warnings = []string{}
	}
	if n.AccountID != "" {
		o.House = map[string]any{"account_id": n.AccountID, "code": n.Code, "unit": n.House, "apartment_id": n.ApartmentID, "apartment": n.Apartment, "customer": n.Customer}
	}
	if n.TR069 {
		o.TR069 = map[string]any{"linked": true, "last_inform": tp(n.TR069Inform), "held_back": n.TR069Held}
	}
	return o
}

func treeOLT(o billing.TreeOLT) apiTreeOLT {
	out := apiTreeOLT{ID: o.Equip.ID, Name: o.Label, Model: o.Equip.Model, Serial: o.Equip.Serial, MAC: o.Equip.MAC, Firmware: o.Equip.Firmware, Status: o.Equip.Status,
		RouterID: o.Equip.SiteID, RouterPort: o.Equip.RouterPort, PONPorts: o.Equip.PONPorts, MgmtIP: o.Equip.MgmtIP, MgmtUser: o.Equip.MgmtUser, HasMgmtPass: o.HasPassword,
		ONUCount: o.ONUCount, OnlineCount: o.Online, WarningCount: o.Warnings, PONs: []apiTreePON{}}
	for _, p := range o.PONs {
		tp := apiTreePON{Port: p.Port, Apartments: []apiTreeApt{}, ONUs: []apiTreeONU{}}
		for _, a := range p.Apartments {
			tp.Apartments = append(tp.Apartments, apiTreeApt{a.ID, a.Name})
		}
		for _, n := range p.ONUs {
			tp.ONUs = append(tp.ONUs, treeONU(n))
		}
		out.PONs = append(out.PONs, tp)
	}
	return out
}

func (s *Server) apiEquipmentTree(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	t := s.svc.Tree(nil)
	routers := []map[string]any{}
	for _, rt := range t.Routers {
		apts, olts := []apiTreeApt{}, []apiTreeOLT{}
		for _, a := range rt.Apartments {
			apts = append(apts, apiTreeApt{a.ID, a.Name})
		}
		for _, o := range rt.OLTs {
			olts = append(olts, treeOLT(o))
		}
		routers = append(routers, map[string]any{"id": rt.Site.ID, "name": rt.Site.Name, "apartments": apts, "olts": olts, "onu_count": rt.ONUCount, "online_count": rt.Online})
	}
	un, onus := []apiTreeOLT{}, []apiTreeONU{}
	for _, o := range t.UnplacedOLTs {
		un = append(un, treeOLT(o))
	}
	for _, n := range t.UnplacedONUs {
		onus = append(onus, treeONU(n))
	}
	writeJSON(w, http.StatusOK, map[string]any{"routers": routers, "unplaced_olts": un, "unplaced_onus": onus})
}

func (s *Server) apiOLTList(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	site := r.URL.Query().Get("site")
	items := []apiEquipment{}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindOLT && (site == "" || e.SiteID == site) {
				items = append(items, apiEquipment{Equipment: e, HasMgmtPass: e.MgmtPass != ""})
			}
		}
	})
	for i := range items {
		items[i].Equipment.MgmtPass = ""
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiOLTGet(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var e store.Equipment
	var ok bool
	s.st.View(func(d *store.Data) { e, ok = d.Equipment[r.PathValue("id")] })
	if !ok || e.Kind != store.KindOLT {
		apiErr(w, http.StatusNotFound, codeNotFound, "Not found.", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.equipDTO(e))
}

func (s *Server) apiOLTSave(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		Name          string           `json:"name"`
		Model         string           `json:"model"`
		Serial        string           `json:"serial"`
		MAC           string           `json:"mac"`
		Firmware      string           `json:"firmware"`
		Notes         string           `json:"notes"`
		SiteID        string           `json:"site_id"`
		RouterPort    string           `json:"router_port"`
		PONPorts      int              `json:"pon_ports"`
		PONApartments map[int][]string `json:"pon_apartments"` // port -> apartment ids; omitted keeps, {} clears
		MgmtIP        string           `json:"mgmt_ip"`
		MgmtUser      string           `json:"mgmt_user"`
		MgmtPass      string           `json:"mgmt_pass"` // write-only; "" keeps the stored one
		ClearMgmtPass bool             `json:"clear_mgmt_pass"`
		Status        string           `json:"status"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	id := r.PathValue("id")
	fields := map[string]string{}
	if strings.TrimSpace(req.Name) == "" {
		fields["name"] = "give the OLT a name"
	}
	if strings.TrimSpace(req.MAC) == "" {
		fields["mac"] = "the OLT's MAC address is required"
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	e, err := s.svc.SaveOLT(billing.OLTInput{ID: id, Name: req.Name, Model: req.Model, Serial: req.Serial, MAC: req.MAC, Firmware: req.Firmware, Notes: req.Notes,
		SiteID: req.SiteID, RouterPort: req.RouterPort, PONPorts: req.PONPorts, PONApartments: req.PONApartments, MgmtIP: req.MgmtIP, MgmtUser: req.MgmtUser,
		MgmtPass: req.MgmtPass, ClearMgmtPass: req.ClearMgmtPass, Status: req.Status})
	if err != nil {
		s.oltErr(w, err, req.MAC)
		return
	}
	s.st.View(func(d *store.Data) { e = d.Equipment[e.ID] }) // SaveOLT hides the sealed value; the DTO shows only that one exists
	c.audit(s, "OLT "+store.OLTLabel(e)+" saved")
	st := http.StatusOK
	if id == "" {
		st = http.StatusCreated
	}
	writeJSON(w, st, s.equipDTO(e))
}

func (s *Server) oltErr(w http.ResponseWriter, err error, mac string) {
	switch {
	case errors.Is(err, billing.ErrNotFound):
		apiErr(w, http.StatusNotFound, codeNotFound, "Not found.", nil)
	case errors.Is(err, billing.ErrDuplicateEquipmentMAC):
		apiErr(w, http.StatusConflict, codeMACInUse, err.Error(), map[string]any{"mac": mac})
	case errors.Is(err, billing.ErrRouterOwnMAC):
		validationErr(w, map[string]string{"mac": err.Error()})
	case errors.Is(err, billing.ErrBadMAC):
		validationErr(w, map[string]string{"mac": "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"})
	case errors.Is(err, billing.ErrNoSite):
		validationErr(w, map[string]string{"site_id": "choose a router"})
	case errors.Is(err, billing.ErrNoDataKey):
		validationErr(w, map[string]string{"mgmt_pass": "the server has no DATA_KEY, so a password cannot be stored"})
	case strings.Contains(err.Error(), "already the ONT of house"):
		apiErr(w, http.StatusConflict, codeMACInUse, err.Error(), map[string]any{"mac": mac})
	case strings.Contains(err.Error(), "PON port"), strings.Contains(err.Error(), "does not serve"), strings.Contains(err.Error(), "apartment"):
		validationErr(w, map[string]string{"pon_apartments": err.Error()})
	case strings.Contains(err.Error(), "router port"):
		validationErr(w, map[string]string{"router_port": err.Error()})
	case strings.Contains(err.Error(), "management address"):
		validationErr(w, map[string]string{"mgmt_ip": err.Error()})
	case strings.Contains(err.Error(), "already in the register"):
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
	default:
		validationErr(w, map[string]string{"olt": err.Error()})
	}
}

// apiONUPlacement: PUT /equipment/{id}/placement {"olt_id": "", "pon_port": 2} puts an ONU under an OLT's PON port.
// A house that its PON port does not serve is a warning in the answer, never a refusal.
func (s *Server) apiONUPlacement(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		OLTID   string `json:"olt_id"`
		PONPort int    `json:"pon_port"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	var cur store.Equipment
	var ok bool
	s.st.View(func(d *store.Data) { cur, ok = d.Equipment[r.PathValue("id")] })
	if !ok || cur.Kind != store.KindONT {
		apiErr(w, http.StatusNotFound, codeNotFound, "Not found.", nil)
		return
	}
	e, err := s.svc.SaveEquipment(billing.EquipmentInput{ID: cur.ID, Kind: cur.Kind, Serial: cur.Serial, GPONSerial: cur.GPONSerial, MAC: cur.MAC, Model: cur.Model,
		Firmware: cur.Firmware, SiteID: cur.SiteID, Notes: cur.Notes, SetPlacement: true, OLTID: req.OLTID, PONPort: req.PONPort})
	if err != nil {
		validationErr(w, map[string]string{"olt_id": err.Error()})
		return
	}
	c.audit(s, "ONU "+e.Serial+" placed on an OLT")
	writeJSON(w, http.StatusOK, s.equipDTO(e))
}
