package acs

import (
	"fmt"
	"regexp"
	"strings"
)

// SOAP / CWMP messages. Parsing is tolerant of the namespace version (cwmp-1-0 to 1-4); building uses the
// namespace the CPE used in its Inform.

const defaultNS = "urn:dslforum-org:cwmp-1-0"

// ParamValue is one name/value of a parameter list. Type is the xsd type used when writing.
type ParamValue struct {
	Name, Value, Type string
}

// Fault is a CWMP fault (SOAP Fault with a cwmp:Fault detail), 9000-9019 and vendor codes.
type Fault struct {
	Code   int
	String string
	// Params are the SetParameterValuesFault entries: the parameters the CPE refused.
	Params []ParamFault
}

// ParamFault is one SetParameterValuesFault of a Fault.
type ParamFault struct {
	Name   string
	Code   int
	String string
}

func (f *Fault) Error() string { return f.Text() }

// Text is the fault as a short plain-words line for a task ("9005 Invalid parameter name"). It names parameters
// only, never values.
func (f *Fault) Text() string {
	t := fmt.Sprintf("%d %s", f.Code, strings.TrimSpace(f.String))
	if f.String == "" {
		t = fmt.Sprintf("%d %s", f.Code, faultName(f.Code))
	}
	if len(f.Params) > 0 {
		var names []string
		for i, p := range f.Params {
			if i >= 3 {
				names = append(names, "...")
				break
			}
			names = append(names, fmt.Sprintf("%s (%d)", p.Name, p.Code))
		}
		t += ": " + strings.Join(names, ", ")
	}
	if len(t) > 300 {
		t = t[:300]
	}
	return t
}

var faultNames = map[int]string{
	9000: "Method not supported", 9001: "Request denied", 9002: "Internal error", 9003: "Invalid arguments",
	9004: "Resources exceeded", 9005: "Invalid parameter name", 9006: "Invalid parameter type", 9007: "Invalid parameter value",
	9008: "Attempt to set a non-writable parameter", 9009: "Notification request rejected", 9010: "Download failure",
	9011: "Upload failure", 9012: "File transfer server authentication failure", 9013: "Unsupported protocol for file transfer",
	9014: "Download failure: unable to join multicast group", 9015: "Download failure: unable to contact file server",
	9016: "Download failure: unable to access file", 9017: "Download failure: unable to complete download",
	9018: "Download failure: file corrupted", 9019: "Download failure: file authentication failure",
}

func faultName(c int) string {
	if n, ok := faultNames[c]; ok {
		return n
	}
	return "fault"
}

// Inform is a parsed Inform request.
type Inform struct {
	Manufacturer, OUI, ProductClass, Serial string
	Events                                  []string // "0 BOOTSTRAP", "2 PERIODIC", ...
	Params                                  []ParamValue
	RetryCount                              int
}

// HasEvent reports whether the Inform carries the event code ("0 BOOTSTRAP" matches code "0").
func (in Inform) HasEvent(code string) bool {
	for _, e := range in.Events {
		if e == code || strings.HasPrefix(e, code+" ") {
			return true
		}
	}
	return false
}

// Param returns the value of an Inform parameter by name.
func (in Inform) Param(name string) (string, bool) {
	for _, p := range in.Params {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

// message is one parsed envelope.
type message struct {
	ID       string
	NS       string // the cwmp namespace the sender used
	Hold     bool
	Method   string // local name of the Body's first element ("Inform", "GetParameterValuesResponse", "Fault", ...)
	Body     *node  // that element
	NoMore   bool
	Envelope *node
}

// parseMessage parses a CWMP envelope.
func parseMessage(b []byte) (*message, error) {
	root, err := parseXML(b)
	if err != nil || root.Name != "Envelope" {
		return nil, errXML
	}
	m := &message{Envelope: root, NS: defaultNS}
	for k, v := range root.Attr {
		if strings.HasPrefix(k, "xmlns") && strings.HasPrefix(v, "urn:dslforum-org:cwmp-1-") {
			m.NS = v
		}
	}
	if h := root.child("Header"); h != nil {
		m.ID = h.childText("ID")
		m.Hold = isTrue(h.childText("HoldRequests"))
		m.NoMore = isTrue(h.childText("NoMoreRequests"))
	}
	body := root.child("Body")
	if body == nil || len(body.Children) == 0 {
		return nil, errXML
	}
	m.Body, m.Method = body.Children[0], body.Children[0].Name
	return m, nil
}

func isTrue(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "1" || s == "true"
}

// parseInform reads the Inform body.
func parseInform(n *node) (Inform, error) {
	var in Inform
	id := n.child("DeviceId")
	if id == nil {
		return in, errXML
	}
	in.Manufacturer, in.OUI, in.ProductClass, in.Serial = id.childText("Manufacturer"), id.childText("OUI"), id.childText("ProductClass"), id.childText("SerialNumber")
	if in.Serial == "" || len(in.Serial) > 64 || len(in.OUI) > 8 || len(in.Manufacturer) > 64 || len(in.ProductClass) > 64 {
		return in, errXML
	}
	if ev := n.child("Event"); ev != nil {
		for _, es := range ev.all("EventStruct") {
			if c := es.childText("EventCode"); c != "" && len(in.Events) < 16 {
				in.Events = append(in.Events, c)
			}
		}
	}
	if pl := n.child("ParameterList"); pl != nil {
		for _, pv := range pl.all("ParameterValueStruct") {
			if len(in.Params) >= 400 {
				break
			}
			v := pv.child("Value")
			val := ""
			if v != nil {
				val = strings.TrimSpace(v.Text)
			}
			in.Params = append(in.Params, ParamValue{Name: pv.childText("Name"), Value: val})
		}
	}
	fmt.Sscanf(n.childText("RetryCount"), "%d", &in.RetryCount)
	return in, nil
}

// parseFault reads a SOAP Fault body (the cwmp:Fault detail holds the codes).
func parseFault(n *node) *Fault {
	f := &Fault{Code: 9002, String: strings.TrimSpace(n.childText("faultstring"))}
	var d *node
	if det := n.child("detail"); det != nil {
		d = det.child("Fault")
	}
	if d == nil {
		return f
	}
	fmt.Sscanf(d.childText("FaultCode"), "%d", &f.Code)
	if s := d.childText("FaultString"); s != "" {
		f.String = s
	}
	for _, sf := range d.all("SetParameterValuesFault") {
		pf := ParamFault{Name: sf.childText("ParameterName"), String: sf.childText("FaultString")}
		fmt.Sscanf(sf.childText("FaultCode"), "%d", &pf.Code)
		f.Params = append(f.Params, pf)
	}
	return f
}

func parseValues(n *node) []ParamValue {
	var out []ParamValue
	pl := n.child("ParameterList")
	for _, pv := range pl.all("ParameterValueStruct") {
		v := ""
		if c := pv.child("Value"); c != nil {
			v = strings.TrimSpace(c.Text)
		}
		out = append(out, ParamValue{Name: pv.childText("Name"), Value: v})
	}
	return out
}

// NameInfo is one entry of a GetParameterNamesResponse.
type NameInfo struct {
	Name     string
	Writable bool
}

func parseNames(n *node) []NameInfo {
	var out []NameInfo
	for _, pi := range n.child("ParameterList").all("ParameterInfoStruct") {
		out = append(out, NameInfo{Name: pi.childText("Name"), Writable: isTrue(pi.childText("Writable"))})
	}
	return out
}

// ---- building ----

func envelope(ns, id, body string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="` + esc(ns) + `">` +
		`<soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">` + esc(id) + `</cwmp:ID></soap-env:Header>` +
		`<soap-env:Body>` + body + `</soap-env:Body></soap-env:Envelope>`)
}

func informResponse(ns, id string) []byte {
	return envelope(ns, id, `<cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>`)
}

func simpleResponse(ns, id, method string) []byte {
	return envelope(ns, id, `<cwmp:`+method+`Response/>`)
}

func getRPCMethodsResponse(ns, id string) []byte {
	return envelope(ns, id, `<cwmp:GetRPCMethodsResponse><MethodList soap-enc:arrayType="xsd:string[5]"><string>Inform</string><string>GetRPCMethods</string><string>TransferComplete</string><string>Kicked</string><string>RequestDownload</string></MethodList></cwmp:GetRPCMethodsResponse>`)
}

func typed(p ParamValue) string {
	t := p.Type
	if t == "" {
		t = "xsd:string"
	}
	return `<ParameterValueStruct><Name>` + esc(p.Name) + `</Name><Value xsi:type="` + t + `">` + esc(p.Value) + `</Value></ParameterValueStruct>`
}

func getParameterValues(ns, id string, names []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:GetParameterValues><ParameterNames soap-enc:arrayType="xsd:string[%d]">`, len(names))
	for _, n := range names {
		b.WriteString(`<string>` + esc(n) + `</string>`)
	}
	b.WriteString(`</ParameterNames></cwmp:GetParameterValues>`)
	return envelope(ns, id, b.String())
}

func getParameterNames(ns, id, path string, nextLevel bool) []byte {
	nl := "0"
	if nextLevel {
		nl = "1"
	}
	return envelope(ns, id, `<cwmp:GetParameterNames><ParameterPath>`+esc(path)+`</ParameterPath><NextLevel>`+nl+`</NextLevel></cwmp:GetParameterNames>`)
}

func setParameterValues(ns, id string, ps []ParamValue, key string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:SetParameterValues><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[%d]">`, len(ps))
	for _, p := range ps {
		b.WriteString(typed(p))
	}
	b.WriteString(`</ParameterList><ParameterKey>` + esc(key) + `</ParameterKey></cwmp:SetParameterValues>`)
	return envelope(ns, id, b.String())
}

func rebootRPC(ns, id, key string) []byte {
	return envelope(ns, id, `<cwmp:Reboot><CommandKey>`+esc(key)+`</CommandKey></cwmp:Reboot>`)
}

func factoryResetRPC(ns, id string) []byte {
	return envelope(ns, id, `<cwmp:FactoryReset/>`)
}

// ---- secrets in logs ----

var sensitiveName = regexp.MustCompile(`(?i)passphrase|password|key|psk|secret|credential`)

// Sensitive says whether a parameter's value must never be logged or shown (passphrases, passwords, keys).
func Sensitive(name string) bool { return sensitiveName.MatchString(name) }

// Shown is the value of a parameter for a log line or a screen: "***" for a sensitive one.
func Shown(name, value string) string {
	if Sensitive(name) {
		return "***"
	}
	return value
}
