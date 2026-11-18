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

// Telegram sends messages through the Bot API.
type Telegram struct {
	Token   string
	BaseURL string // default https://api.telegram.org
	HTTP    *http.Client
}

type tgSend struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

// Send posts to /bot<token>/sendMessage. m.To is the chat id.
func (t *Telegram) Send(ctx context.Context, m Message) (string, error) {
	base := strings.TrimRight(t.BaseURL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	body, _ := json.Marshal(tgSend{ChatID: m.To, Text: m.Text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return "", Permanent("telegram request: %v", scrub(err, t.Token))
	}
	req.Header.Set("Content-Type", "application/json")
	cl := t.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("telegram: %v", scrub(err, t.Token))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode == 200 && out.OK {
		return fmt.Sprint(out.Result.MessageID), nil
	}
	e := fmt.Errorf("telegram: %d %s", resp.StatusCode, out.Description)
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return "", AuthError{e} // bad token, or the bot was blocked or removed from the chat
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return "", e
	case resp.StatusCode >= 400:
		return "", PermanentError{e}
	}
	return "", e
}

// scrub removes a secret from an error text (net/http errors include the URL).
func scrub(err error, secret string) string {
	s := err.Error()
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[token]")
	}
	return s
}
