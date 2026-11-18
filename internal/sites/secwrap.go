package sites

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

var rosUARe = regexp.MustCompile(`(?i)^\s*routeros\s+(\d+\.\d+(?:\.\d+)?)`)

// ROSVersionFromUA reads the RouterOS version out of a /tool fetch
// User-Agent ("RouterOS 7.25.1"). It returns "" for anything else (RouterOS 6
// sends "Mikrotik/6.x Fetch"), which selects the old section wrapper.
func ROSVersionFromUA(ua string) string {
	m := rosUARe.FindStringSubmatch(ua)
	if m == nil {
		return ""
	}
	return m[1]
}

// rosAtLeast reports whether a "major.minor[.patch]" version is >= major.minor.
func rosAtLeast(ver string, major, minor int) bool {
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return false
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return a > major || (a == major && b >= minor)
}

// sectionFuncs returns the template functions that wrap a script section so
// its failure is recorded in $report with the router's own error text.
//
//	{{secOpen 3 "REST API" true}} ... {{secClose 3 "REST API" true}}
//
// fatal=true stops the script (the outer handler removes the file and posts
// the report); fatal=false counts the failure in $fails and goes on.
// RouterOS 7.13+ has ":onerror e in={...} do={...}", which gives $e; older
// versions get ":do {...} on-error={...}" with no error text.
// BENCH-CHECK: the exact ":onerror" syntax on 7.25.1 (the form is isolated here).
func sectionFuncs(rosVersion string) template.FuncMap {
	modern := rosAtLeast(rosVersion, 7, 13)
	return template.FuncMap{
		"secOpen": func(n int, name string, fatal bool) string {
			if modern {
				return ":onerror e in={"
			}
			return ":do {"
		},
		"secClose": func(n int, name string, fatal bool) string {
			label := fmt.Sprintf("%d %s", n, name)
			var rep, tail string
			if modern {
				rep = fmt.Sprintf(`:set report ($report . "%s: " . $e . "\n")`, label)
				if fatal {
					tail = fmt.Sprintf(`:set abort ("ftn: section %s failed: " . $e); :error $abort`, label)
				} else {
					tail = fmt.Sprintf(`:set fails ($fails + 1); :put ("ftn: section %s failed, continuing: " . $e)`, label)
				}
				return "} do={ " + rep + "; " + tail + " }"
			}
			rep = fmt.Sprintf(`:set report ($report . "%s: failed (no error text before RouterOS 7.13)\n")`, label)
			if fatal {
				tail = fmt.Sprintf(`:set abort "ftn: section %s failed"; :error $abort`, label)
			} else {
				tail = fmt.Sprintf(`:set fails ($fails + 1); :put "ftn: section %s failed, continuing"`, label)
			}
			return "} on-error={ " + rep + "; " + tail + " }"
		},
	}
}

// reportPostLine is the router-side line that sends $report to the server. It is
// harmless when it fails; the report is cut to 1500 characters first (the server caps it at 2 KB).
// BENCH-CHECK: http-method=post with http-data and output=none on 7.25.1.
func reportPostLine(base, token string, httpsBase, noCheck bool) string {
	chk := ""
	if httpsBase {
		chk = " check-certificate=yes"
		if noCheck {
			chk = " check-certificate=no"
		}
	}
	return fmt.Sprintf(`:if ([:len $report] > 1500) do={ :set report [:pick $report 0 1500] }; :do { /tool/fetch url="%s/j/%s/report" http-method=post http-header-field="Content-Type: text/plain" http-data=("result " . $result . " ros " . $ver . "\n" . $report) output=none%s } on-error={ :put "   could not send the join report to the server" }`,
		base, token, chk)
}
