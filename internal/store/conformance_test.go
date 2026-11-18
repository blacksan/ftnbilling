package store_test

import (
	"testing"

	"ispbill/internal/store/storetest"
)

// TestConformance runs the shared suite on the backend chosen by TEST_STORE
// (JSON by default; the Postgres leg lives in pgstore's tests).
func TestConformance(t *testing.T) { storetest.Run(t) }
