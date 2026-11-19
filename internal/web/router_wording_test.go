package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// D8/D9 of docs/release-plan-routers.md: the product word is "router". This
// guard fails when a Go string literal that reaches a user (admin messages,
// API errors, alert labels, manager errors) says "building". Templates are
// guarded separately (W7); the three tenant portal phrases keep "building".
//
// Allowed literals are exact strings and need a reason.
var buildingAllowed = map[string]string{
	"building":                        "CSV column name and the ?kind= key of the search redirect: wire names, not prose",
	"mac_other_building":              "the API error code is fixed by the apartments spec (wire name, not prose)",
	"already exists in this building": "matches the text of an error raised by internal/billing (billing is not renamed in P0)",
}

func TestNoUserFacingBuildingInGoStrings(t *testing.T) {
	var files []string
	for _, g := range []string{"*.go", "../alerts/alerts.go", "../sites/manager.go"} {
		m, _ := filepath.Glob(g)
		files = append(files, m...)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(af, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil || !strings.Contains(strings.ToLower(s), "building") {
				return true
			}
			if _, ok := buildingAllowed[s]; ok {
				return true
			}
			t.Errorf("%s: user-facing \"building\" in %q: say \"router\" (or add it to buildingAllowed with a reason)", fset.Position(bl.Pos()), s)
			return true
		})
	}
	if checked < 20 {
		t.Fatalf("guard looked at only %d files", checked)
	}
}
