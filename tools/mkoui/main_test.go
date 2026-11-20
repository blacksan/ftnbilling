package main

import (
	"bytes"
	"go/format"
	"strings"
	"testing"
)

const sample = `OUI/MA-L			Organization
company_id			Organization
				Address

00-E0-4C   (hex)		REALTEK SEMICONDUCTOR CORP.
00E04C     (base 16)		REALTEK SEMICONDUCTOR CORP.
				No. 2, Innovation Road II
6C-1F-F7   (hex)		Ugreen Group Limited
00-00-AA   (hex)		PRIVATE
`

func TestParseAndRender(t *testing.T) {
	m, err := parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["00:E0:4C"] != "REALTEK SEMICONDUCTOR CORP." || m["6C:1F:F7"] != "Ugreen Group Limited" {
		t.Fatalf("%v", m)
	}
	var b bytes.Buffer
	render(m, &b)
	if _, err := format.Source(b.Bytes()); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, b.String())
	}
}
