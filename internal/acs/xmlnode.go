// Package acs is the built-in TR-069 (CWMP) auto-configuration server of ftnbilling. It speaks SOAP 1.1 over
// HTTP to the customers' ONUs through the router's management VLAN and the WireGuard tunnel, keeps a record of
// each device, and runs queued actions (change Wi-Fi, reboot) inside the device's sessions.
package acs

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// Limits of the XML a CPE may send. The parser builds a small tree and refuses anything bigger: no DTD, no
// entities (Go's decoder expands none besides the five predefined ones), a bounded depth and node count.
const (
	maxBody  = 256 << 10
	maxDepth = 24
	maxNodes = 20000
)

var errXML = errors.New("bad XML")

// node is one element: namespaces are dropped (CPEs of cwmp-1-0 to 1-4 differ only there).
type node struct {
	Name     string // local name
	Attr     map[string]string
	Text     string
	Children []*node
}

// child returns the first child with the local name.
func (n *node) child(name string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// childText is the trimmed text of the first child with the name.
func (n *node) childText(name string) string {
	if c := n.child(name); c != nil {
		return strings.TrimSpace(c.Text)
	}
	return ""
}

func (n *node) all(name string) []*node {
	var out []*node
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// parseXML parses a document into a tree, with the limits above.
func parseXML(b []byte) (*node, error) {
	if len(b) > maxBody {
		return nil, errXML
	}
	// A DTD or entity declaration has no place in CWMP: refuse before parsing (no entity tricks).
	if bytes.Contains(b, []byte("<!DOCTYPE")) || bytes.Contains(b, []byte("<!ENTITY")) || bytes.Contains(b, []byte("<!doctype")) {
		return nil, errXML
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	var stack []*node
	var root *node
	count := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errXML
		}
		switch t := tok.(type) {
		case xml.StartElement:
			count++
			if count > maxNodes || len(stack) >= maxDepth {
				return nil, errXML
			}
			n := &node{Name: t.Name.Local}
			for _, a := range t.Attr {
				if n.Attr == nil {
					n.Attr = map[string]string{}
				}
				key := a.Name.Local
				if a.Name.Space == "xmlns" {
					key = "xmlns:" + a.Name.Local
				}
				n.Attr[key] = a.Value
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.Children = append(p.Children, n)
			} else if root == nil {
				root = n
			} else {
				return nil, errXML
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errXML
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				n := stack[len(stack)-1]
				if len(n.Text) < 8192 {
					n.Text += string(t)
				}
			}
		}
	}
	if root == nil || len(stack) != 0 {
		return nil, errXML
	}
	return root, nil
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
