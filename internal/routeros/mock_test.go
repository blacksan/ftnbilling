package routeros

import (
	"context"
	"testing"
)

// RouterOS refuses a second address-list row with the same list and address.
func TestMockRefusesDuplicateAddressListRow(t *testing.T) {
	m := NewMock()
	ctx := context.Background()
	const p = "/ip/firewall/address-list"
	if _, err := m.AddRow(ctx, p, map[string]string{"list": "ftn-undo", "address": "127.0.1.1", "comment": "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddRow(ctx, p, map[string]string{"list": "ftn-undo", "address": "127.0.1.1", "comment": "b"}); err == nil {
		t.Fatal("a duplicate (list, address) row must be refused")
	}
	if _, err := m.AddRow(ctx, p, map[string]string{"list": "ftn-undo", "address": "127.0.1.2", "comment": "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddRow(ctx, p, map[string]string{"list": "other", "address": "127.0.1.1"}); err != nil {
		t.Fatal(err)
	}
}
