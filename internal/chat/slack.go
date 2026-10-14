package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/secrets"
)

// DefaultSlackAPI è l'indirizzo dell'API Web di Slack.
const DefaultSlackAPI = "https://slack.com/api"

// SlackAPI chiama l'API Web di Slack con il bot token, letto dai segreti a ogni uso.
type SlackAPI struct {
	URL      string
	TokenRef string
	Secrets  secrets.Provider
	Client   HTTPDoer
}

func (s *SlackAPI) endpoint(method string) string {
	base := s.URL
	if base == "" {
		base = DefaultSlackAPI
	}
	return strings.TrimRight(base, "/") + "/" + method
}

// call esegue un metodo dell'API; ok=false nella risposta è un errore.
func (s *SlackAPI) call(ctx context.Context, method string, body any, out any) error {
	tok, err := s.Secrets.Resolve(ctx, s.TokenRef)
	if err != nil {
		return fmt.Errorf("token di Slack: %w", err)
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	raw, err := postJSON(ctx, s.Client, s.endpoint(method), map[string]string{"Authorization": "Bearer " + strings.TrimSpace(tok)}, body, &resp)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	if !resp.OK {
		return fmt.Errorf("slack %s: %s", method, resp.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Post invia un messaggio al canale e ne restituisce il riferimento.
func (s *SlackAPI) Post(ctx context.Context, channel, text string, blocks []any) (*Ref, error) {
	var out struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := s.call(ctx, "chat.postMessage", map[string]any{"channel": channel, "text": text, "blocks": blocks, "unfurl_links": false}, &out); err != nil {
		return nil, err
	}
	return &Ref{Channel: out.Channel, TS: out.TS}, nil
}

// Update sostituisce il contenuto di un messaggio inviato.
func (s *SlackAPI) Update(ctx context.Context, ref Ref, text string, blocks []any) error {
	return s.call(ctx, "chat.update", map[string]any{"channel": ref.Channel, "ts": ref.TS, "text": text, "blocks": blocks}, nil)
}

// UserEmail legge l'email di un utente (scope users:read.email).
func (s *SlackAPI) UserEmail(ctx context.Context, user string) (string, error) {
	tok, err := s.Secrets.Resolve(ctx, s.TokenRef)
	if err != nil {
		return "", fmt.Errorf("token di Slack: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint("users.info")+"?"+url.Values{"user": {user}}.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tok))
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		User  struct {
			Deleted bool `json:"deleted"`
			Profile struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", fmt.Errorf("slack users.info: %s", out.Error)
	}
	if out.User.Deleted || out.User.Profile.Email == "" {
		return "", errors.New("utente Slack senza email")
	}
	return out.User.Profile.Email, nil
}

// postJSON invia un corpo JSON e restituisce la risposta; uno stato diverso da 2xx è un errore.
func postJSON(ctx context.Context, client HTTPDoer, u string, headers map[string]string, body, out any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "manent")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		// L'URL può contenere un token (webhook): l'errore riporta solo l'host.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := ""
		if resp.StatusCode == http.StatusTooManyRequests {
			msg = " (retry-after " + resp.Header.Get("Retry-After") + "s)"
		}
		return nil, fmt.Errorf("%s: status %d%s", req.URL.Host, resp.StatusCode, msg)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("%s: risposta non valida: %w", req.URL.Host, err)
		}
	}
	return raw, nil
}

// VerifySlackSignature verifica la firma di una richiesta di Slack (v0, HMAC-SHA256 di
// "v0:<timestamp>:<corpo>") e rifiuta le richieste più vecchie di 5 minuti.
func VerifySlackSignature(secret string, header http.Header, body []byte, now time.Time) error {
	ts := header.Get("X-Slack-Request-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("timestamp di Slack mancante")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return errors.New("richiesta di Slack scaduta")
	}
	want := "v0=" + hmacHex(secret, "v0:"+ts+":"+string(body))
	if !hmacEqual(want, header.Get("X-Slack-Signature")) {
		return errors.New("firma di Slack non valida")
	}
	return nil
}
