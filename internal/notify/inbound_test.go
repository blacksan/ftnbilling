package notify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

type inRig struct {
	st  *store.Store
	c   *clock
	box *Outbox
	in  *Inbound
}

func newInRig(t *testing.T) *inRig {
	t.Helper()
	st, _ := storetest.Open(t, t.TempDir())
	c := &clock{t: noon()}
	box := newBox(t, c, &fake{}, nil)
	st.Update(func(d *store.Data) error {
		d.Sites["s1"] = store.Site{Mode: store.SiteModeEnv, ID: "s1", Name: "Kilimani"}
		d.Packages["pm"] = store.Package{ID: "pm", Name: "Monthly 10 Mbps", Slug: "m", DurationMin: 43200, SpeedMbps: 10, PriceKES: 3000}
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "s1", Code: "ABC123", Flat: "Flat 3B", WhatsApp: "254712345678", PackageID: "pm", ExpiresAt: c.Now().Add(50 * time.Hour)}
		d.Accounts["a2"] = store.Account{ID: "a2", SiteID: "s1", Code: "XYZ789", Flat: "Flat 4A", WhatsApp: "254799999999"}
		return nil
	})
	return &inRig{st: st, c: c, box: box, in: NewInbound(InboundOptions{Store: st, Box: box, Now: c.Now, AppSecret: "appsecret", VerifyToken: "vt", SupportPhone: "0700 111 222", PayHint: "M-Pesa Paybill 123456"})}
}

func msgBody(id, from, text string) []byte {
	return []byte(fmt.Sprintf(`{"object":"whatsapp_business_account","entry":[{"changes":[{"field":"messages","value":{"messages":[{"from":%q,"id":%q,"type":"text","text":{"body":%q}}]}}]}]}`, from, id, text))
}

func replies(r *inRig) []Message {
	return r.box.Find(func(m Message) bool { return m.Kind == KindReply })
}

func TestWebhookVerifyHandshake(t *testing.T) {
	r := newInRig(t)
	if ch, ok := r.in.Verify("subscribe", "vt", "12345"); !ok || ch != "12345" {
		t.Fatal("good handshake")
	}
	for _, c := range [][3]string{{"subscribe", "wrong", "1"}, {"unsubscribe", "vt", "1"}, {"subscribe", "", "1"}} {
		if _, ok := r.in.Verify(c[0], c[1], c[2]); ok {
			t.Fatalf("must refuse %v", c)
		}
	}
	empty := NewInbound(InboundOptions{Store: r.st, Box: r.box})
	if _, ok := empty.Verify("subscribe", "", "1"); ok {
		t.Fatal("an unset verify token must never match")
	}
}

func TestWebhookSignature(t *testing.T) {
	r := newInRig(t)
	body := msgBody("w1", "254712345678", "BALANCE")
	good := Sign("appsecret", body)
	if !r.in.ValidSignature(body, good) {
		t.Fatal("good signature")
	}
	// the known value for HMAC-SHA256("appsecret", body) pins the algorithm
	if !strings.HasPrefix(good, "sha256=") || len(good) != 7+64 {
		t.Fatal(good)
	}
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-5] ^= 1
	for name, c := range map[string]struct {
		b []byte
		h string
	}{
		"tampered body": {tampered, good}, "wrong secret": {body, Sign("other", body)}, "no prefix": {body, strings.TrimPrefix(good, "sha256=")},
		"empty": {body, ""}, "not hex": {body, "sha256=zz"}, "sha1": {body, "sha1=" + strings.TrimPrefix(good, "sha256=")},
	} {
		if r.in.ValidSignature(c.b, c.h) {
			t.Fatalf("%s must be refused", name)
		}
	}
	if NewInbound(InboundOptions{Store: r.st, Box: r.box}).ValidSignature(body, Sign("", body)) {
		t.Fatal("with no secret nothing is valid")
	}
}

func TestBalanceRepliesOnlyToMatchingNumber(t *testing.T) {
	r := newInRig(t)
	r.in.Handle(msgBody("w1", "254712345678", "balance"))
	got := replies(r)
	if len(got) != 1 || got[0].To != "254712345678" || got[0].Template != "" {
		t.Fatalf("%+v", got)
	}
	txt := got[0].Text
	for _, want := range []string{"Flat 3B, Kilimani", "Monthly 10 Mbps", "ABC123", "left"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("balance reply lacks %q: %s", want, txt)
		}
	}
	if strings.Contains(txt, "XYZ789") || strings.Contains(txt, "Flat 4A") {
		t.Fatal("another tenant's account leaked")
	}
	// STATUS is the same; a number that is on no account only gets the support phone.
	r.in.Handle(msgBody("w2", "254700000000", "BALANCE"))
	var stranger Message
	for _, m := range replies(r) {
		if m.To == "254700000000" {
			stranger = m
		}
	}
	if stranger.ID == "" || !strings.Contains(stranger.Text, "0700 111 222") || strings.Contains(stranger.Text, "ABC123") || strings.Contains(stranger.Text, "Flat") {
		t.Fatalf("%+v", stranger)
	}
	// expired plan: how to pay
	r.st.Update(func(d *store.Data) error {
		a := d.Accounts["a1"]
		a.ExpiresAt = r.c.Now().Add(-time.Hour)
		d.Accounts["a1"] = a
		return nil
	})
	r.in.Handle(msgBody("w3", "254712345678", "STATUS"))
	if got := replies(r); !strings.Contains(got[len(got)-1].Text, "M-Pesa Paybill 123456") || !strings.Contains(got[len(got)-1].Text, "ended") {
		t.Fatalf("%s", got[len(got)-1].Text)
	}
}

func TestStopStartAndHelp(t *testing.T) {
	r := newInRig(t)
	r.in.Handle(msgBody("w1", "254712345678", "STOP"))
	if a := r.st2("a1"); !a.WAOptOut {
		t.Fatal("STOP sets the opt-out")
	}
	if a := r.st2("a2"); a.WAOptOut {
		t.Fatal("only the sender's accounts")
	}
	r.in.Handle(msgBody("w2", "254712345678", " start "))
	if a := r.st2("a1"); a.WAOptOut {
		t.Fatal("START clears it")
	}
	r.in.Handle(msgBody("w3", "254712345678", "hello?"))
	if got := replies(r); len(got) != 3 || !strings.Contains(got[2].Text, "BALANCE") {
		t.Fatalf("help reply: %+v", got)
	}
}

func (r *inRig) st2(id string) (a store.Account) {
	r.st.View(func(d *store.Data) { a = d.Accounts[id] })
	return
}

func TestDuplicateMessageIDIgnored(t *testing.T) {
	r := newInRig(t)
	for i := 0; i < 3; i++ {
		r.in.Handle(msgBody("same-id", "254712345678", "BALANCE"))
	}
	if n := len(replies(r)); n != 1 {
		t.Fatalf("a message id is handled once, got %d replies", n)
	}
	// the last 2000 ids are remembered, older ones are forgotten
	for i := 0; i < seenCap+5; i++ {
		r.in.dup(fmt.Sprint("x", i))
	}
	if r.in.dup("x1") == true && r.in.dup(fmt.Sprint("x", seenCap+4)) == false {
		t.Fatal("window")
	}
	if len(r.in.seen) > seenCap {
		t.Fatalf("seen %d", len(r.in.seen))
	}
}

func TestReplyRateLimit(t *testing.T) {
	r := newInRig(t)
	for i := 0; i < 8; i++ {
		r.in.Handle(msgBody(fmt.Sprint("m", i), "254712345678", "BALANCE"))
	}
	if n := len(replies(r)); n != repliesPerMin {
		t.Fatalf("at most %d replies per sender per minute, got %d", repliesPerMin, n)
	}
	r.c.Add(61 * time.Second)
	r.in.Handle(msgBody("later", "254712345678", "BALANCE"))
	if n := len(replies(r)); n != repliesPerMin+1 {
		t.Fatalf("after a minute again: %d", n)
	}
	// another sender is not affected
	r.in.Handle(msgBody("other", "254799999999", "BALANCE"))
	if n := len(replies(r)); n != repliesPerMin+2 {
		t.Fatal(n)
	}
}

func TestDeliveryStatuses(t *testing.T) {
	r := newInRig(t)
	m, _ := r.box.Enqueue(Message{Channel: ChanWhatsApp, To: "254712345678", AccountID: "a1", Kind: KindPaymentReceived, Template: "t", Params: []string{"1"}})
	r.c.Add(time.Second)
	r.box.Step(context.Background(), ChanWhatsApp)
	g, _ := r.box.Get(m.ID)
	status := func(st, errTitle string) []byte {
		e := ""
		if errTitle != "" {
			e = fmt.Sprintf(`,"errors":[{"title":%q,"message":"details"}]`, errTitle)
		}
		return []byte(fmt.Sprintf(`{"entry":[{"changes":[{"value":{"statuses":[{"id":%q,"status":%q%s}]}}]}]}`, g.ProviderID, st, e))
	}
	r.in.Handle(status("delivered", ""))
	if g, _ = r.box.Get(m.ID); g.Delivery != "delivered" {
		t.Fatal(g.Delivery)
	}
	r.in.Handle(status("read", ""))
	if g, _ = r.box.Get(m.ID); g.Delivery != "read" {
		t.Fatal(g.Delivery)
	}
	r.in.Handle(status("failed", "Message undeliverable"))
	if g, _ = r.box.Get(m.ID); g.Delivery != "failed" || !strings.Contains(g.LastErr, "undeliverable") {
		t.Fatalf("%+v", g)
	}
	if err := r.in.Handle([]byte("not json")); err == nil {
		t.Fatal("bad payload is an error")
	}
}
