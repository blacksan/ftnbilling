package web

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"ispbill/internal/billing"
)

// An account MAC that is the router's own comes back as a validation error with the plain text.
func TestRouterOwnMACErrorIsAValidationError(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).billingErr(rec, fmt.Errorf("attach: %w", billing.ErrRouterOwnMAC), "38:32:7A:07:B6:CD")
	if rec.Code != 422 && rec.Code != 400 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "That MAC belongs to the router itself") {
		t.Fatalf("body %s", rec.Body.String())
	}
}
