package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WhatsApp sends template messages (and free-form session replies) through
// the Meta Cloud API.
type WhatsApp struct {
	Token         string
	PhoneNumberID string
	APIVersion    string // e.g. v23.0
	Lang          string // default template language, "en"
	BaseURL       string // default https://graph.facebook.com
	HTTP          *http.Client
}

type waParam struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type waComponent struct {
	Type       string    `json:"type"`
	Parameters []waParam `json:"parameters"`
}
type waLang struct {
	Code string `json:"code"`
}
type waTemplate struct {
	Name       string        `json:"name"`
	Language   waLang        `json:"language"`
	Components []waComponent `json:"components,omitempty"`
}
type waText struct {
	Body string `json:"body"`
}
type waBody struct {
	MessagingProduct string      `json:"messaging_product"`
	To               string      `json:"to"`
	Type             string      `json:"type"`
	Template         *waTemplate `json:"template,omitempty"`
	Text             *waText     `json:"text,omitempty"`
}

// Body builds the JSON for a message: a template message when m.Template is
// set, else a free-form text (only valid inside a 24-hour customer session).
func (w *WhatsApp) Body(m Message) ([]byte, error) {
	b := waBody{MessagingProduct: "whatsapp", To: strings.TrimPrefix(m.To, "+")}
	if m.Template != "" {
		lang := m.Lang
		if lang == "" {
			lang = w.Lang
		}
		if lang == "" {
			lang = "en"
		}
		t := &waTemplate{Name: m.Template, Language: waLang{Code: lang}}
		if len(m.Params) > 0 {
			c := waComponent{Type: "body"}
			for _, p := range m.Params {
				c.Parameters = append(c.Parameters, waParam{Type: "text", Text: CleanParam(p)})
			}
			t.Components = []waComponent{c}
		}
		b.Type, b.Template = "template", t
	} else {
		b.Type, b.Text = "text", &waText{Body: m.Text}
	}
	return json.Marshal(b)
}

// Send posts to /<version>/<phone-number-id>/messages.
func (w *WhatsApp) Send(ctx context.Context, m Message) (string, error) {
	base := strings.TrimRight(w.BaseURL, "/")
	if base == "" {
		base = "https://graph.facebook.com"
	}
	ver := w.APIVersion
	if ver == "" {
		ver = "v23.0"
	}
	body, err := w.Body(m)
	if err != nil {
		return "", Permanent("whatsapp body: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/%s/%s/messages", base, ver, w.PhoneNumberID), bytes.NewReader(body))
	if err != nil {
		return "", Permanent("whatsapp request: %v", scrub(err, w.Token))
	}
	req.Header.Set("Authorization", "Bearer "+w.Token)
	req.Header.Set("Content-Type", "application/json")
	cl := w.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("whatsapp: %v", scrub(err, w.Token))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var out struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
			Sub     int    `json:"error_subcode"`
			Data    struct {
				Details string `json:"details"`
			} `json:"error_data"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode/100 == 2 && len(out.Messages) > 0 {
		return out.Messages[0].ID, nil
	}
	detail := out.Error.Message
	if out.Error.Data.Details != "" {
		detail += " (" + out.Error.Data.Details + ")"
	}
	e := fmt.Errorf("whatsapp: %d code %d: %s", resp.StatusCode, out.Error.Code, detail)
	return "", classifyWA(resp.StatusCode, out.Error.Code, e)
}

// classifyWA maps a Cloud API error to retry behaviour.
func classifyWA(status, code int, e error) error {
	switch {
	case status == 401 || code == 190 || code == 102: // bad or expired token
		return AuthError{e}
	case code == 80007 || code == 130429 || code == 131056 || code == 4 || code == 17 || code == 32 || code == 613 || code == 131000:
		return e // rate limited or a Meta-side hiccup: retry later
	case status == 429 || status >= 500:
		return e
	case code == 10 || code == 200 || code == 299 || code == 368 || code == 131031: // permission or account blocked
		return AuthError{e}
	case status >= 400: // recipient not on WhatsApp, template mismatch, number not allowed, ...
		return PermanentError{e}
	}
	return e
}
