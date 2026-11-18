package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWhatsAppExactJSON(t *testing.T) {
	var gotPath, gotAuth, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody, gotCT = r.URL.Path, r.Header.Get("Authorization"), string(b), r.Header.Get("Content-Type")
		w.Write([]byte(`{"messaging_product":"whatsapp","contacts":[{"input":"254712345678","wa_id":"254712345678"}],"messages":[{"id":"wamid.ABC"}]}`))
	}))
	defer srv.Close()
	w := &WhatsApp{Token: "TOK", PhoneNumberID: "1234567890", APIVersion: "v23.0", Lang: "en", BaseURL: srv.URL}
	id, err := w.Send(context.Background(), Message{To: "254712345678", Template: TplAdminAlert,
		Params: []string{"PROBLEM", "Building offline", "Kilimani\nBlock A", "down   for 5m\t(since 12:00)", "18 Nov 12:00"}})
	if err != nil || id != "wamid.ABC" {
		t.Fatalf("%q %v", id, err)
	}
	want := `{"messaging_product":"whatsapp","to":"254712345678","type":"template","template":{"name":"ftn_admin_alert","language":{"code":"en"},` +
		`"components":[{"type":"body","parameters":[{"type":"text","text":"PROBLEM"},{"type":"text","text":"Building offline"},` +
		`{"type":"text","text":"Kilimani Block A"},{"type":"text","text":"down for 5m (since 12:00)"},{"type":"text","text":"18 Nov 12:00"}]}]}}`
	if gotBody != want {
		t.Fatalf("body\n got %s\nwant %s", gotBody, want)
	}
	if gotPath != "/v23.0/1234567890/messages" || gotAuth != "Bearer TOK" || gotCT != "application/json" {
		t.Fatalf("%s %s %s", gotPath, gotAuth, gotCT)
	}
}

func TestWhatsAppParamCleaning(t *testing.T) {
	long := strings.Repeat("x", 300)
	if got := CleanParam(long); len([]rune(got)) != 200 {
		t.Fatal(len(got))
	}
	if CleanParam("  \n\t ") != "-" {
		t.Fatal("empty parameter must become a dash, Meta rejects empty text")
	}
	if CleanParam("a \r\n\t  b") != "a b" {
		t.Fatal(CleanParam("a \r\n\t  b"))
	}
}

func TestWhatsAppErrorClasses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		auth   bool
		perm   bool
	}{
		{401, `{"error":{"message":"Invalid OAuth access token","code":190}}`, true, false},
		{400, `{"error":{"message":"Recipient phone number not in allowed list","code":131030}}`, false, true},
		{400, `{"error":{"message":"Number of parameters does not match","code":132000}}`, false, true},
		{400, `{"error":{"message":"Too many messages","code":130429}}`, false, false},
		{500, `{"error":{"message":"oops","code":1}}`, false, false},
		{429, `{}`, false, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		w := &WhatsApp{Token: "TOK", PhoneNumberID: "1", BaseURL: srv.URL}
		_, err := w.Send(context.Background(), Message{To: "254712345678", Template: "t"})
		srv.Close()
		if err == nil || isAuth(err) != c.auth || isPermanent(err) != c.perm {
			t.Fatalf("%d %s: err=%v auth=%v perm=%v", c.status, c.body, err, isAuth(err), isPermanent(err))
		}
	}
}

func TestWhatsAppTextSession(t *testing.T) {
	w := &WhatsApp{PhoneNumberID: "1"}
	b, _ := w.Body(Message{To: "+254712345678", Text: "Your plan ends 20 Nov"})
	if string(b) != `{"messaging_product":"whatsapp","to":"254712345678","type":"text","text":{"body":"Your plan ends 20 Nov"}}` {
		t.Fatal(string(b))
	}
}

func TestTelegramExactJSON(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer srv.Close()
	tg := &Telegram{Token: "123:ABC", BaseURL: srv.URL}
	id, err := tg.Send(context.Background(), Message{To: "-100200300", Text: "Kilimani is offline"})
	if err != nil || id != "77" {
		t.Fatalf("%q %v", id, err)
	}
	if gotPath != "/bot123:ABC/sendMessage" || gotBody != `{"chat_id":"-100200300","text":"Kilimani is offline"}` {
		t.Fatalf("%s %s", gotPath, gotBody)
	}
}

func TestTelegramErrorClassesAndTokenScrubbed(t *testing.T) {
	for status, c := range map[int][2]bool{401: {true, false}, 403: {true, false}, 400: {false, true}, 429: {false, false}, 502: {false, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"ok":false,"description":"nope"}`))
		}))
		_, err := (&Telegram{Token: "T", BaseURL: srv.URL}).Send(context.Background(), Message{To: "1", Text: "x"})
		srv.Close()
		if err == nil || isAuth(err) != c[0] || isPermanent(err) != c[1] {
			t.Fatalf("%d: %v", status, err)
		}
	}
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close() // connection refused: the error text contains the URL and so the token
	_, err := (&Telegram{Token: "SECRETTOKEN", BaseURL: url}).Send(context.Background(), Message{To: "1", Text: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") || isPermanent(err) || isAuth(err) {
		t.Fatalf("%v", err)
	}
}

// ---- SMTP against an in-test server ----------------------------------------

type smtpRec struct {
	mu                   sync.Mutex
	auth, from, data     string
	rcpt                 []string
	startTLS             bool
	rejectAuth, rejectTo bool
}

// serveSMTP speaks just enough SMTP for net/smtp's client.
func serveSMTP(t *testing.T, ln net.Listener, rec *smtpRec, offerTLS *tls.Config) {
	t.Helper()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var conn net.Conn = c
			r := bufio.NewReader(conn)
			say := func(s string) { io.WriteString(conn, s+"\r\n") }
			say("220 test ESMTP")
			inData := false
			var data strings.Builder
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if inData {
					if line == "." {
						inData = false
						rec.mu.Lock()
						rec.data = data.String()
						rec.mu.Unlock()
						say("250 queued")
						continue
					}
					data.WriteString(line + "\r\n")
					continue
				}
				up := strings.ToUpper(line)
				switch {
				case strings.HasPrefix(up, "EHLO"):
					io.WriteString(conn, "250-test\r\n")
					if offerTLS != nil && !rec.startTLS {
						io.WriteString(conn, "250-STARTTLS\r\n")
					}
					io.WriteString(conn, "250 AUTH PLAIN\r\n")
				case up == "STARTTLS":
					say("220 go ahead")
					tc := tls.Server(conn, offerTLS)
					if err := tc.Handshake(); err != nil {
						return
					}
					conn = tc
					r = bufio.NewReader(conn)
					rec.mu.Lock()
					rec.startTLS = true
					rec.mu.Unlock()
				case strings.HasPrefix(up, "AUTH PLAIN"):
					if rec.rejectAuth {
						say("535 5.7.8 authentication failed")
						continue
					}
					rec.mu.Lock()
					rec.auth = strings.TrimSpace(line[len("AUTH PLAIN"):])
					rec.mu.Unlock()
					say("235 ok")
				case strings.HasPrefix(up, "MAIL FROM:"):
					rec.mu.Lock()
					rec.from = line[len("MAIL FROM:"):]
					rec.mu.Unlock()
					say("250 ok")
				case strings.HasPrefix(up, "RCPT TO:"):
					if rec.rejectTo {
						say("550 5.1.1 no such user")
						continue
					}
					rec.mu.Lock()
					rec.rcpt = append(rec.rcpt, line[len("RCPT TO:"):])
					rec.mu.Unlock()
					say("250 ok")
				case up == "DATA":
					inData = true
					say("354 go")
				case up == "QUIT":
					say("221 bye")
					return
				default:
					say("250 ok")
				}
			}
		}()
	}
}

func testCert(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

func listenPort(t *testing.T, ln net.Listener) int { return ln.Addr().(*net.TCPAddr).Port }

func checkMail(t *testing.T, rec *smtpRec) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	wantAuth := base64.StdEncoding.EncodeToString([]byte("\x00ops\x00pw"))
	if rec.auth != wantAuth {
		t.Fatalf("auth %q", rec.auth)
	}
	if rec.from != "<alerts@ftn.example>" || len(rec.rcpt) != 2 || rec.rcpt[0] != "<a@x.example>" || rec.rcpt[1] != "<b@x.example>" {
		t.Fatalf("envelope %q %q", rec.from, rec.rcpt)
	}
	for _, want := range []string{"From: Fast Tech Net <alerts@ftn.example>\r\n", "To: a@x.example, b@x.example\r\n", "Subject: [ALERT] Kilimani offline\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n", "\r\n\r\nKilimani has been offline for 5 minutes.\r\nLine two.\r\n"} {
		if !strings.Contains(rec.data, want) {
			t.Fatalf("missing %q in\n%s", want, rec.data)
		}
	}
}

func testMsg() Message {
	return Message{ID: "m1", To: "a@x.example, b@x.example", Subject: "[ALERT] Kilimani offline", Text: "Kilimani has been offline for 5 minutes.\nLine two."}
}

func TestSMTPPlainLoopback(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	rec := &smtpRec{}
	go serveSMTP(t, ln, rec, nil)
	s := &SMTP{Host: "127.0.0.1", Port: listenPort(t, ln), User: "ops", Pass: "pw", From: "Fast Tech Net <alerts@ftn.example>"}
	if _, err := s.Send(context.Background(), testMsg()); err != nil {
		t.Fatal(err)
	}
	checkMail(t, rec)
}

func TestSMTPStartTLSAndImplicitTLS(t *testing.T) {
	srvCfg, pool := testCert(t)
	// STARTTLS (any port other than 465)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	rec := &smtpRec{}
	go serveSMTP(t, ln, rec, srvCfg)
	s := &SMTP{Host: "127.0.0.1", Port: listenPort(t, ln), User: "ops", Pass: "pw", From: "Fast Tech Net <alerts@ftn.example>", TLS: &tls.Config{RootCAs: pool}}
	if _, err := s.Send(context.Background(), testMsg()); err != nil {
		t.Fatal(err)
	}
	if !rec.startTLS {
		t.Fatal("STARTTLS was not used")
	}
	checkMail(t, rec)
	// Implicit TLS (what port 465 does)
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	tln := tls.NewListener(raw, srvCfg)
	defer tln.Close()
	rec3 := &smtpRec{}
	go serveSMTP(t, tln, rec3, nil)
	s3 := &SMTP{Host: "127.0.0.1", Port: listenPort(t, raw), Implicit: true, User: "ops", Pass: "pw", From: "Fast Tech Net <alerts@ftn.example>", TLS: &tls.Config{RootCAs: pool}}
	if _, err := s3.Send(context.Background(), testMsg()); err != nil {
		t.Fatal(err)
	}
	checkMail(t, rec3)
	// A server certificate that is not trusted must be refused.
	rec2 := &smtpRec{}
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln2.Close()
	go serveSMTP(t, ln2, rec2, srvCfg)
	s2 := &SMTP{Host: "127.0.0.1", Port: listenPort(t, ln2), User: "ops", Pass: "pw", From: "alerts@ftn.example"}
	if _, err := s2.Send(context.Background(), testMsg()); err == nil || rec2.auth != "" {
		t.Fatalf("untrusted certificate must fail before AUTH: %v", err)
	}
}

func TestSMTPErrorClasses(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	rec := &smtpRec{rejectAuth: true}
	go serveSMTP(t, ln, rec, nil)
	s := &SMTP{Host: "127.0.0.1", Port: listenPort(t, ln), User: "ops", Pass: "bad", From: "alerts@ftn.example"}
	if _, err := s.Send(context.Background(), testMsg()); err == nil || !isAuth(err) {
		t.Fatalf("535 must be an auth error: %v", err)
	}
	rec.rejectAuth, rec.rejectTo = false, true
	if _, err := s.Send(context.Background(), testMsg()); err == nil || !isPermanent(err) {
		t.Fatalf("550 recipient must be permanent: %v", err)
	}
	ln.Close()
	if _, err := s.Send(context.Background(), testMsg()); err == nil || isPermanent(err) || isAuth(err) {
		t.Fatalf("connection refused must be transient: %v", err)
	}
	if _, err := s.Send(context.Background(), Message{To: "not an address", Text: "x"}); !isPermanent(err) {
		t.Fatalf("bad recipient: %v", err)
	}
	var te error = Auth("x")
	if !errors.Is(te, te) {
		t.Fatal()
	}
}
