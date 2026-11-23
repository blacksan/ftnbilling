package acs

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

// ---- the fixture: a store with one router, one house, a service and a fake CPE ----

var testKey = bytes.Repeat([]byte{7}, 32)

type fixture struct {
	t     *testing.T
	st    *store.Store
	svc   *Service
	acs   *httptest.Server
	logs  *syncBuf
	now   time.Time
	mu    sync.Mutex
	crSrv *httptest.Server
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

const testAccountMAC = "AA:BB:CC:00:11:22"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, logs: &syncBuf{}, now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	st, err := storetest.Open(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.st = st
	if err := st.Update(func(d *store.Data) error {
		d.Sites["s1"] = store.Site{ID: "s1", Name: "Block A", Mode: store.SiteModeWG, TunnelIP: "10.99.0.2", ONUMgmtN: 3, CreatedAt: f.now}
		d.Sites["s2"] = store.Site{ID: "s2", Name: "Block B", Mode: store.SiteModeWG, TunnelIP: "10.99.0.3", ONUMgmtN: 4, CreatedAt: f.now}
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "s1", Code: "ABC234", Flat: "B4", Unit: "B4", MAC: testAccountMAC, CreatedAt: f.now}
		d.Accounts["a2"] = store.Account{ID: "a2", SiteID: "s1", Code: "DEF567", Flat: "B5", Unit: "B5", MAC: "AA:BB:CC:00:11:33", CreatedAt: f.now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ACSListen: "10.99.0.1:7547", ACSCRPorts: []int{7547, 30005}, ACSInformInterval: 300, ACSMaxPerRouter: 200}
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := New(Options{Store: st, Cfg: cfg, Log: log, DataKey: testKey, Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		ClientIP: func(r *http.Request) (netip.Addr, bool) {
			a, err := netip.ParseAddr(r.Header.Get("X-Test-IP"))
			return a, err == nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	dbg = func() string {
		var s string
		f.st.View(func(d *store.Data) {
			for _, t := range d.ONUTasks {
				s += fmt.Sprintf("%+v\n", t)
			}
		})
		return s + f.logs.String()
	}
	f.acs = httptest.NewServer(svc.Handler())
	t.Cleanup(func() { f.acs.Close(); svc.Wait() })
	return f
}

func (f *fixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func (f *fixture) onuBySerial(serial string) store.ONU {
	var o store.ONU
	f.st.View(func(d *store.Data) { o, _ = d.ONUBySerial("", serial) })
	return o
}

func (f *fixture) lastTask(onuID string) store.ONUTask {
	var t store.ONUTask
	f.st.View(func(d *store.Data) {
		if ts := d.ONUTasksOf(onuID); len(ts) > 0 {
			t = ts[0]
		}
	})
	return t
}

// ---- the fake CPE ----

type cpe struct {
	t    *testing.T
	f    *fixture
	ip   string
	root string

	Manufacturer, OUI, ProductClass, Serial string

	mu       sync.Mutex
	params   map[string]string // the data model: every name that exists, with its value
	readonly map[string]bool
	// accept, when not nil, says which names a Set may write (others fault with 9005).
	refuse   func(name string) bool
	detailed bool // the 9005 fault names the parameters

	acsUser, acsPass string // what this CPE logs in with
	crUser, crPass   string // what the ACS must log in with for a connection request
	crPort           int
	crHits           int
	crWG             sync.WaitGroup
	crSrv            *httptest.Server
	crEvents         []string // events sent in the session a connection request starts

	rpcs  []string            // the RPCs the ACS sent, by name
	sets  []map[string]string // each SetParameterValues the ACS sent
	setID []string
}

func newZTE(t *testing.T, f *fixture, serial, ip string) *cpe {
	c := &cpe{t: t, f: f, ip: ip, root: store.RootTR098, Manufacturer: "ZTE", OUI: "0019C6", ProductClass: "F670L", Serial: serial,
		params: map[string]string{}, readonly: map[string]bool{}, acsUser: "", acsPass: "", crPort: 7547, detailed: true}
	r := c.root
	c.set(r+"DeviceInfo.SoftwareVersion", "V9.0.10")
	c.set(r+"DeviceInfo.HardwareVersion", "V1.0")
	c.set(r+"ManagementServer.ConnectionRequestURL", "http://"+ip+":7547/cr")
	c.set(r+"ManagementServer.ConnectionRequestUsername", "admin")
	c.set(r+"ManagementServer.ConnectionRequestPassword", "admin")
	c.set(r+"ManagementServer.PeriodicInformEnable", "0")
	c.set(r+"ManagementServer.PeriodicInformInterval", "86400")
	for _, w := range []struct {
		n    string
		freq string
		std  string
		ch   string
		ssid string
		en   string
	}{{"1", "2.4GHz", "b,g,n", "6", "ZTE-2G", "1"}, {"2", "2.4GHz", "b,g,n", "6", "guest-2G", "0"}, {"5", "5GHz", "a,n,ac", "36", "ZTE-5G", "1"}} {
		p := r + "LANDevice.1.WLANConfiguration." + w.n + "."
		c.set(p+"SSID", w.ssid)
		c.set(p+"Enable", w.en)
		c.set(p+"Channel", w.ch)
		c.set(p+"Standard", w.std)
		c.set(p+"OperatingFrequencyBand", w.freq)
		c.set(p+"BeaconType", "None")
		c.set(p+"IEEE11iAuthenticationMode", "None")
		c.set(p+"IEEE11iEncryptionModes", "None")
		c.set(p+"PreSharedKey.1.KeyPassphrase", "factorypass")
	}
	c.set(r+"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.MACAddress", testAccountMAC)
	c.set(r+"WANDevice.1.WANConnectionDevice.2.WANPPPConnection.1.MACAddress", "AA:BB:CC:00:99:99")
	return c
}

func newTR181(t *testing.T, f *fixture, serial, ip string) *cpe {
	c := &cpe{t: t, f: f, ip: ip, root: store.RootTR181, Manufacturer: "Acme", OUI: "ACE001", ProductClass: "GenericONU", Serial: serial,
		params: map[string]string{}, readonly: map[string]bool{}, crPort: 7547, detailed: true}
	c.set("Device.DeviceInfo.SoftwareVersion", "2.1")
	c.set("Device.ManagementServer.ConnectionRequestURL", "http://"+ip+":7547/cr")
	c.set("Device.ManagementServer.ConnectionRequestUsername", "")
	c.set("Device.ManagementServer.ConnectionRequestPassword", "")
	c.set("Device.ManagementServer.PeriodicInformEnable", "0")
	c.set("Device.ManagementServer.PeriodicInformInterval", "86400")
	c.set("Device.WiFi.Radio.1.OperatingFrequencyBand", "2.4GHz")
	c.set("Device.WiFi.Radio.1.Channel", "11")
	c.set("Device.WiFi.Radio.2.OperatingFrequencyBand", "5GHz")
	c.set("Device.WiFi.Radio.2.Channel", "44")
	for i, ssid := range []string{"Acme-2G", "Acme-5G"} {
		n := fmt.Sprint(i + 1)
		c.set("Device.WiFi.SSID."+n+".SSID", ssid)
		c.set("Device.WiFi.SSID."+n+".Enable", "1")
		c.set("Device.WiFi.SSID."+n+".LowerLayers", "Device.WiFi.Radio."+n)
		c.set("Device.WiFi.AccessPoint."+n+".SSIDReference", "Device.WiFi.SSID."+n+".")
		c.set("Device.WiFi.AccessPoint."+n+".Security.ModeEnabled", "None")
		c.set("Device.WiFi.AccessPoint."+n+".Security.KeyPassphrase", "")
	}
	c.set("Device.Ethernet.Link.1.MACAddress", testAccountMAC)
	c.set("Device.Ethernet.Link.2.MACAddress", "AA:BB:CC:00:99:98")
	return c
}

// newHuawei is a TR-098 box that only knows the X_HW passphrase name and has no IEEE11iEncryptionModes.
func newHuawei(t *testing.T, f *fixture, serial, ip string) *cpe {
	c := newZTE(t, f, serial, ip)
	c.Manufacturer, c.OUI, c.ProductClass = "Huawei Technologies Co., Ltd", "00E0FC", "HG8245H"
	for k := range c.params {
		if strings.Contains(k, "PreSharedKey.1.KeyPassphrase") {
			delete(c.params, k)
		}
	}
	for _, n := range []string{"1", "2", "5"} {
		p := c.root + "LANDevice.1.WLANConfiguration." + n + "."
		c.set(p+"X_HW_KeyPassphrase", "factorypass")
		delete(c.params, p+"IEEE11iEncryptionModes")
	}
	return c
}

func (c *cpe) set(name, v string) { c.params[name] = v }
func (c *cpe) get(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.params[name]
}

func (c *cpe) wlan(n string) string { return c.root + "LANDevice.1.WLANConfiguration." + n + "." }

// children is GetParameterNames NextLevel over the flat map.
func (c *cpe) children(path string) ([]NameInfo, bool) {
	seen := map[string]bool{}
	var out []NameInfo
	exists := false
	for name := range c.params {
		if !strings.HasPrefix(name, path) || name == path {
			continue
		}
		exists = true
		rest := name[len(path):]
		if i := strings.Index(rest, "."); i >= 0 {
			rest = rest[:i+1]
		}
		if !seen[rest] {
			seen[rest] = true
			out = append(out, NameInfo{Name: path + rest, Writable: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, exists
}

func (c *cpe) env(id, body string) []byte {
	return []byte(`<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-2" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
		`<soap:Header><cwmp:ID soap:mustUnderstand="1">` + id + `</cwmp:ID></soap:Header><soap:Body>` + body + `</soap:Body></soap:Envelope>`)
}

func (c *cpe) inform(events ...string) []byte {
	var ev strings.Builder
	for _, e := range events {
		code, _, _ := strings.Cut(e, " ")
		ev.WriteString("<EventStruct><EventCode>" + e + "</EventCode><CommandKey></CommandKey></EventStruct>")
		_ = code
	}
	var pl strings.Builder
	for _, n := range []string{"DeviceInfo.SoftwareVersion", "DeviceInfo.HardwareVersion", "ManagementServer.ConnectionRequestURL"} {
		if v, ok := c.params[c.root+n]; ok {
			pl.WriteString(`<ParameterValueStruct><Name>` + c.root + n + `</Name><Value xsi:type="xsd:string">` + esc(v) + `</Value></ParameterValueStruct>`)
		}
	}
	for _, e := range events {
		if strings.HasPrefix(e, "4 ") { // a value change carries the changed names
			for k, v := range c.params {
				if strings.HasSuffix(k, ".SSID") {
					pl.WriteString(`<ParameterValueStruct><Name>` + k + `</Name><Value xsi:type="xsd:string">` + esc(v) + `</Value></ParameterValueStruct>`)
				}
			}
		}
	}
	return c.env("1", `<cwmp:Inform><DeviceId><Manufacturer>`+c.Manufacturer+`</Manufacturer><OUI>`+c.OUI+`</OUI><ProductClass>`+c.ProductClass+`</ProductClass><SerialNumber>`+c.Serial+`</SerialNumber></DeviceId>`+
		`<Event>`+ev.String()+`</Event><MaxEnvelopes>1</MaxEnvelopes><CurrentTime>2026-11-20T09:00:00Z</CurrentTime><RetryCount>0</RetryCount><ParameterList>`+pl.String()+`</ParameterList></cwmp:Inform>`)
}

func (c *cpe) faultXML(id string, code int, str string, pf []ParamFault) []byte {
	var d strings.Builder
	for _, p := range pf {
		d.WriteString(fmt.Sprintf("<SetParameterValuesFault><ParameterName>%s</ParameterName><FaultCode>%d</FaultCode><FaultString>%s</FaultString></SetParameterValuesFault>", p.Name, p.Code, p.String))
	}
	return c.env(id, `<soap:Fault><faultcode>Client</faultcode><faultstring>CWMP fault</faultstring><detail><cwmp:Fault><FaultCode>`+fmt.Sprint(code)+`</FaultCode><FaultString>`+str+`</FaultString>`+d.String()+`</cwmp:Fault></detail></soap:Fault>`)
}

// handle answers one ACS request.
func (c *cpe) handle(m *message) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rpcs = append(c.rpcs, m.Method)
	switch m.Method {
	case "GetParameterNames":
		path := m.Body.childText("ParameterPath")
		nl := isTrue(m.Body.childText("NextLevel"))
		if !nl {
			return c.faultXML(m.ID, 9003, "only NextLevel is supported here", nil)
		}
		kids, ok := c.children(path)
		if !ok {
			return c.faultXML(m.ID, 9005, "Invalid parameter name", nil)
		}
		var b strings.Builder
		for _, k := range kids {
			b.WriteString("<ParameterInfoStruct><Name>" + k.Name + "</Name><Writable>1</Writable></ParameterInfoStruct>")
		}
		return c.env(m.ID, `<cwmp:GetParameterNamesResponse><ParameterList>`+b.String()+`</ParameterList></cwmp:GetParameterNamesResponse>`)
	case "GetParameterValues":
		var b strings.Builder
		for _, n := range m.Body.child("ParameterNames").all("string") {
			name := strings.TrimSpace(n.Text)
			v, ok := c.params[name]
			if !ok {
				return c.faultXML(m.ID, 9005, "Invalid parameter name", nil)
			}
			b.WriteString(`<ParameterValueStruct><Name>` + name + `</Name><Value xsi:type="xsd:string">` + esc(v) + `</Value></ParameterValueStruct>`)
		}
		return c.env(m.ID, `<cwmp:GetParameterValuesResponse><ParameterList>`+b.String()+`</ParameterList></cwmp:GetParameterValuesResponse>`)
	case "SetParameterValues":
		got := map[string]string{}
		var bad []ParamFault
		var order []string
		for _, pv := range m.Body.child("ParameterList").all("ParameterValueStruct") {
			name := pv.childText("Name")
			val := ""
			if v := pv.child("Value"); v != nil {
				val = v.Text
			}
			got[name] = val
			order = append(order, name)
			if _, ok := c.params[name]; !ok || (c.refuse != nil && c.refuse(name)) {
				bad = append(bad, ParamFault{Name: name, Code: 9005, String: "Invalid parameter name"})
			}
		}
		if len(bad) > 0 {
			if !c.detailed {
				bad = nil
			}
			return c.faultXML(m.ID, 9003, "Invalid arguments", bad)
		}
		c.sets = append(c.sets, got) // only what the device accepted
		c.setID = append(c.setID, m.Body.childText("ParameterKey"))
		for k, v := range got {
			c.params[k] = v
		}
		return c.env(m.ID, `<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>`)
	case "Reboot":
		return c.env(m.ID, `<cwmp:RebootResponse/>`)
	case "FactoryReset":
		return c.env(m.ID, `<cwmp:FactoryResetResponse/>`)
	}
	return c.faultXML(m.ID, 9000, "Method not supported", nil)
}

// session runs one CWMP session against the ACS (digest login, Inform, then answer every request until it ends).
func (c *cpe) session(events ...string) error {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	post := func(body []byte) (*http.Response, []byte, error) {
		do := func(auth string) (*http.Response, []byte, error) {
			req, _ := http.NewRequest("POST", c.f.acs.URL+"/", bytes.NewReader(body))
			req.Header.Set("X-Test-IP", c.ip)
			req.Header.Set("Content-Type", "text/xml")
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := hc.Do(req)
			if err != nil {
				return nil, nil, err
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp, b, nil
		}
		resp, b, err := do("")
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == 401 {
			auth, ok := clientDigest(resp.Header.Get("WWW-Authenticate"), c.acsUser, c.acsPass, "POST", "/")
			if !ok {
				return nil, nil, fmt.Errorf("no digest challenge")
			}
			return do(auth)
		}
		return resp, b, nil
	}
	if c.acsUser == "" {
		u, p, _ := c.f.svc.Credentials()
		c.acsUser, c.acsPass = u, p
	}
	resp, b, err := post(c.inform(events...))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("InformResponse")) {
		return fmt.Errorf("Inform answered %d %s", resp.StatusCode, b)
	}
	var next []byte // empty = send an empty POST
	for i := 0; i < 100; i++ {
		resp, b, err = post(next)
		if err != nil {
			return err
		}
		if resp.StatusCode == 204 || len(bytes.TrimSpace(b)) == 0 {
			return nil
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("status %d: %s", resp.StatusCode, b)
		}
		m, err := parseMessage(b)
		if err != nil {
			return err
		}
		next = c.handle(m)
	}
	return fmt.Errorf("session did not end")
}

// listen starts the connection-request endpoint; the service's HTTP client is pointed at it.
func (c *cpe) listen() {
	c.crSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		user, pass := c.crUser, c.crPass
		c.mu.Unlock()
		if user != "" {
			f := parseDigest(r.Header.Get("Authorization"))
			ok := f != nil && f["username"] == user && f["nonce"] == "cpe-nonce" && f["uri"] == "/cr"
			if ok {
				ha1 := md5hexT(user + ":" + f["realm"] + ":" + pass)
				ha2 := md5hexT("GET:" + f["uri"])
				ok = f["response"] == md5hexT(ha1+":"+f["nonce"]+":"+f["nc"]+":"+f["cnonce"]+":"+f["qop"]+":"+ha2)
			}
			if !ok {
				w.Header().Set("WWW-Authenticate", `Digest realm="cpe", qop="auth", nonce="cpe-nonce", algorithm=MD5`)
				w.WriteHeader(401)
				return
			}
		}
		c.mu.Lock()
		c.crHits++
		ev := c.crEvents
		c.mu.Unlock()
		w.WriteHeader(200)
		if ev == nil {
			ev = []string{"6 CONNECTION REQUEST"}
		}
		c.crWG.Add(1)
		go func() {
			defer c.crWG.Done()
			if err := c.session(ev...); err != nil {
				c.t.Errorf("session after a connection request: %v", err)
			}
		}()
	}))
	c.t.Cleanup(func() { c.crWG.Wait(); c.crSrv.Close() })
	addr := c.crSrv.Listener.Addr().String()
	c.f.svc.client = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
}

func md5hexT(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

func (c *cpe) hits() int { c.mu.Lock(); defer c.mu.Unlock(); return c.crHits }

// lastSet returns the newest SetParameterValues the ACS sent.
func (c *cpe) lastSet() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sets) == 0 {
		return nil
	}
	return c.sets[len(c.sets)-1]
}

func (c *cpe) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.rpcs {
		if m == method {
			n++
		}
	}
	return n
}

// eventually polls for a condition.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\n%s", what, dbg())
}

// allSets merges every SetParameterValues the ACS sent.
func (c *cpe) allSets() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for _, m := range c.sets {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

var dbg = func() string { return "" }
