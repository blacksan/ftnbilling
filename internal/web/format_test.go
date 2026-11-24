package web

import "testing"

func TestDisplayFormats(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 115448: "115,448", -2500: "-2,500", 1234567: "1,234,567"} {
		if got := groupInt(in); got != want {
			t.Errorf("groupInt(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"254712345678": "0712 345 678", "+254712345678": "0712 345 678", "0712345678": "0712 345 678",
		"712345678": "0712 345 678", "0712 345 678": "0712 345 678", "": "", "n/a": "n/a", "12345": "12345",
	} {
		if got := displayPhone(in); got != want {
			t.Errorf("displayPhone(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"mpesa-c2b": "M-Pesa Paybill", "mpesa-stk": "M-Pesa prompt", "mpesa-manual": "M-Pesa (recorded by hand)",
		"cash": "Cash", "voucher": "Voucher", "": "", "bank-transfer": "Bank transfer"} {
		if got := payMethodName(in); got != want {
			t.Errorf("payMethodName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"Flat 1A": "House 1A", "flat 2": "House 2", "Block C-12A": "House Block C-12A", "B6": "House B6", "": ""} {
		if got := houseLabel(in); got != want {
			t.Errorf("houseLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if kindName("ont") != "ONU" || kindName("olt") != "OLT" || kindName("switch") != "Switch" {
		t.Error("kindName")
	}
}
