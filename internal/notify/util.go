package notify

import "strings"

// scrub removes a secret from an error text (net/http errors include the URL).
func scrub(err error, secret string) string {
	s := err.Error()
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[token]")
	}
	return s
}
