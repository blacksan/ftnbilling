package billing

// ieeeOUI is the IEEE MA-L registry (24-bit prefixes, "AA:BB:CC" -> registered name). It is empty in the
// source tree until somebody runs tools/mkoui on the file the IEEE publishes:
//
//	curl -o oui.txt https://standards-oui.ieee.org/oui/oui.txt
//	go run ./tools/mkoui oui.txt > internal/billing/oui_ieee.go
//
// The server never fetches it: the data is compiled in, and the curated table (oui.go) names the makers
// the operator meets most.
var ieeeOUI = map[string]string{}
