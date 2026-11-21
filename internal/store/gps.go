package store

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ErrBadGPS is returned by ParseGPS for text that holds no usable point.
var ErrBadGPS = errors.New(`GPS location not understood: paste "lat, lng" such as -1.2921, 36.8219, or a Google Maps link that contains the coordinates`)

// ErrGPSDecimals is returned for a pair where a number has no decimal point: "1,5" could be
// (1, 5) or 1.5 with a comma decimal, and a wrong guess puts the pin in another country.
var ErrGPSDecimals = errors.New("GPS location is ambiguous: use dot decimals, e.g. -1.2921, 36.8219")

const gpsNum = `[-+]?\d{1,3}(?:\.\d+)?`

var (
	gpsPair = regexp.MustCompile(`^\s*(` + gpsNum + `)\s*[,;\s]\s*(` + gpsNum + `)\s*$`)
	// Google Maps URL shapes: /@lat,lng,zoom  ·  !3dlat!4dlng (place data)  ·  q= ll= query= center= destination= with lat,lng.
	gpsAt   = regexp.MustCompile(`@(` + gpsNum + `),(` + gpsNum + `)`)
	gpsData = regexp.MustCompile(`!3d(` + gpsNum + `)!4d(` + gpsNum + `)`)
)

// ParseGPS reads what people paste from Google Maps: "-1.2921, 36.8219" (comma,
// semicolon or space between the numbers) or a Google Maps URL with @lat,lng,
// !3dlat!4dlng or a q/query/ll/center/destination=lat,lng parameter. Empty
// input gives (nil, nil, nil): no point. The range is checked with ValidGPS.
func ParseGPS(s string) (lat, lng *float64, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil, nil
	}
	var a, b string
	if m := gpsPair.FindStringSubmatch(s); m != nil {
		a, b = m[1], m[2]
	} else if strings.HasPrefix(strings.ToLower(s), "http://") || strings.HasPrefix(strings.ToLower(s), "https://") {
		a, b = gpsFromURL(s)
	}
	if a == "" {
		return nil, nil, ErrBadGPS
	}
	// Google Maps always gives decimals. A number without a '.' ("1,5", "-1,29") is refused.
	if !strings.Contains(a, ".") || !strings.Contains(b, ".") {
		return nil, nil, ErrGPSDecimals
	}
	la, e1 := strconv.ParseFloat(a, 64)
	lo, e2 := strconv.ParseFloat(b, 64)
	if e1 != nil || e2 != nil {
		return nil, nil, ErrBadGPS
	}
	if err := ValidGPS(&la, &lo); err != nil {
		return nil, nil, err
	}
	return &la, &lo, nil
}

func gpsFromURL(raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	// The exact place pin (!3d..!4d..) beats the map centre (@..), which beats search parameters.
	for _, re := range []*regexp.Regexp{gpsData, gpsAt} {
		if m := re.FindStringSubmatch(u.EscapedPath() + "?" + u.RawQuery + "#" + u.Fragment); m != nil {
			return m[1], m[2]
		}
	}
	q := u.Query()
	for _, k := range []string{"q", "query", "ll", "center", "destination", "daddr"} {
		if m := gpsPair.FindStringSubmatch(strings.ReplaceAll(q.Get(k), "+", " ")); m != nil {
			return m[1], m[2]
		}
	}
	return "", ""
}
