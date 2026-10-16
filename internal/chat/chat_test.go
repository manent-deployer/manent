package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/service"
)

type mapSecrets map[string]string

func (m mapSecrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := m[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("segreto %s assente", ref)
}

// fakeSlack emulates the Slack Web API, an incoming webhook, a Teams webhook and response_url.
type fakeSlack struct {
	mu       sync.Mutex
	posts    []map[string]any
	updates  []map[string]any
	webhooks map[string][]map[string]any
	replies  []string
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	auth := r.Header.Get("Authorization")
	switch r.URL.Path {
	case "/api/chat.postMessage", "/api/chat.update":
		if auth != "Bearer xoxb-test" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		if r.URL.Path == "/api/chat.update" {
			f.updates = append(f.updates, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		f.posts = append(f.posts, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C42", "ts": "1700000000.000100"})
	case "/api/users.info":
		email := map[string]string{"U3": "Dave@Example.com"}[r.URL.Query().Get("user")]
		if email == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"profile": map[string]any{"email": email}}})
	case "/respond":
		f.replies = append(f.replies, body["text"].(string))
	default:
		if f.webhooks == nil {
			f.webhooks = map[string][]map[string]any{}
		}
		f.webhooks[r.URL.Path] = append(f.webhooks[r.URL.Path], body)
	}
}

func setup(t *testing.T) (*Notifier, *fakeSlack, *httptest.Server) {
	f := &fakeSlack{}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	sec := mapSecrets{"bot": "xoxb-test", "hook-slack": srv.URL + "/services/T/B/x", "hook-teams": srv.URL + "/workflows/teams"}
	n := &Notifier{
		PublicURL: "https://manent.example.com/", Language: "it", Secrets: sec, Client: srv.Client(),
		Slack:  &SlackAPI{URL: srv.URL + "/api", TokenRef: "bot", Secrets: sec, Client: srv.Client()},
		Logger: slog.New(slog.DiscardHandler),
		Routes: []Route{
			{Name: "prod", Applications: []string{"*"}, Environments: []string{"prod"}, SlackChannel: "#deploy"},
			{Name: "audit", Applications: []string{"shop"}, Environments: []string{"*"}, DryRun: true, SlackWebhookRef: "hook-slack", Categories: []string{CategoryDeployment}},
			{Name: "teams", Applications: []string{"*"}, Environments: []string{"prod*"}, TeamsWebhookRef: "hook-teams", Categories: []string{CategoryApproval}},
		},
	}
	return n, f, srv
}

func message(event string) Message {
	return Message{
		Event: event, DeploymentID: "shop-1.4.0", Application: "shop", Environment: "prod", Artifact: "shop", Version: "1.4.0",
		RequestedBy: "oidc:alice", Change: &Change{Number: "CHG0031337", URL: "https://itsm.example.com/chg"},
	}
}

func TestDestinazioni(t *testing.T) {
	n, _, _ := setup(t)
	if got := n.Match("shop", "prod", false); !slices.Equal(got, []string{"prod", "audit", "teams"}) {
		t.Errorf("shop/prod: %v", got)
	}
	if got := n.Match("shop", "test", true); !slices.Equal(got, []string{"audit"}) {
		t.Errorf("dry-run: %v", got)
	}
	if got := n.Match("cart", "prod-eu", false); !slices.Equal(got, []string{"teams"}) {
		t.Errorf("cart/prod-eu: %v", got)
	}
	if !n.Wants("prod", EventApprovalRequested) || n.Wants("audit", EventApprovalRequested) || n.Wants("teams", EventStarted) || n.Wants("rimossa", EventStarted) {
		t.Error("filtro per categoria")
	}
}

func TestSlackETeams(t *testing.T) {
	n, f, _ := setup(t)
	ctx := context.Background()
	ref, err := n.Send(ctx, "prod", message(EventStarted), nil)
	if err != nil || ref == nil || ref.Channel != "C42" {
		t.Fatalf("invio: %+v, %v", ref, err)
	}
	final := message(EventFailed)
	final.Error, final.Rollback = "step deploy: timeout", "completed"
	if _, err := n.Send(ctx, "prod", final, ref); err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 || len(f.updates) != 1 || f.updates[0]["ts"] != ref.TS || f.posts[0]["channel"] != "#deploy" {
		t.Fatalf("post %v, update %v", f.posts, f.updates)
	}
	upd := rawJSON(f.updates[0])
	for _, want := range []string{"Deploy fallito: shop 1.4.0 → prod", "step deploy: timeout", "Rollback automatico", "<https://itsm.example.com/chg|CHG0031337>", "https://manent.example.com/deployments/shop-1.4.0"} {
		if !strings.Contains(upd, want) {
			t.Errorf("messaggio aggiornato senza %q: %s", want, upd)
		}
	}

	// Approval request: Approve and Reject buttons with the deploy ID.
	req := message(EventApprovalRequested)
	req.Approval = &Approval{Required: 2, Decisions: []Decision{{Approver: "oidc:bob", Approve: true}}, ExpiresAt: time.Now().Add(time.Hour), Change: true}
	if _, err := n.Send(ctx, "prod", req, nil); err != nil {
		t.Fatal(err)
	}
	blocks := rawJSON(f.posts[1]["blocks"])
	for _, want := range []string{`"action_id":"manent_approve"`, `"action_id":"manent_reject"`, `"value":"shop-1.4.0"`, "1/2 + approvazione del change", "oidc:bob ha approvato"} {
		if !strings.Contains(blocks, want) {
			t.Errorf("approvazione senza %q: %s", want, blocks)
		}
	}

	// Slack webhook: no approval buttons (there is no app receiving the clicks).
	if ref, err := n.Send(ctx, "audit", message(EventSucceeded), nil); err != nil || ref != nil {
		t.Fatalf("webhook Slack: %+v, %v", ref, err)
	}
	hook := f.webhooks["/services/T/B/x"]
	if len(hook) != 1 || !strings.Contains(fmt.Sprint(hook[0]["text"]), "Deploy riuscito") {
		t.Errorf("webhook Slack: %v", hook)
	}

	// Teams: Adaptive Card with the link to decide in the web interface.
	if _, err := n.Send(ctx, "teams", req, nil); err != nil {
		t.Fatal(err)
	}
	card := rawJSON(f.webhooks["/workflows/teams"][0])
	for _, want := range []string{`"contentType":"application/vnd.microsoft.card.adaptive"`, `"type":"AdaptiveCard"`, `"Action.OpenUrl"`, "Valuta in manent", `"title":"Change CHG0031337"`} {
		if !strings.Contains(card, want) {
			t.Errorf("card di Teams senza %q: %s", want, card)
		}
	}
}

func TestErroriDiInvio(t *testing.T) {
	n, _, _ := setup(t)
	n.Slack.TokenRef = "bot-errato"
	if _, err := n.Send(context.Background(), "prod", message(EventStarted), nil); err == nil {
		t.Error("token assente: atteso errore")
	}
	n.Secrets = mapSecrets{"bot": "xoxb-scaduto", "hook-slack": "http://hooks.example.com/x"}
	n.Slack.Secrets, n.Slack.TokenRef = n.Secrets, "bot"
	if _, err := n.Send(context.Background(), "prod", message(EventStarted), nil); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("token rifiutato: %v", err)
	}
	// A non-https webhook is a permanent error: a retry cannot fix the configured URL.
	if _, err := n.Send(context.Background(), "audit", message(EventStarted), nil); err == nil || !strings.Contains(err.Error(), "https") || !errors.Is(err, pipeline.ErrPermanent) {
		t.Errorf("webhook non https: %v", err)
	}
}

// fakeDecider records the decisions with the identity in the context.
type fakeDecider struct {
	mu        sync.Mutex
	decisions []string
	err       error
}

func (d *fakeDecider) Decide(ctx context.Context, id string, r service.DecisionRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := auth.FromContext(ctx)
	d.decisions = append(d.decisions, fmt.Sprintf("%s %s %v %s %v", p.String(), id, r.Approve, r.Comment, p.Groups))
	return d.err
}

func signed(t *testing.T, h http.Handler, secret string, ts time.Time, payload map[string]any) int {
	t.Helper()
	raw, _ := json.Marshal(payload)
	body := url.Values{"payload": {string(raw)}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/integrations/slack/interactions", strings.NewReader(body))
	stamp := strconv.FormatInt(ts.Unix(), 10)
	req.Header.Set("X-Slack-Request-Timestamp", stamp)
	req.Header.Set("X-Slack-Signature", "v0="+hmacHex(secret, "v0:"+stamp+":"+body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func click(user, action, responseURL string) map[string]any {
	return map[string]any{
		"type": "block_actions", "user": map[string]any{"id": user, "username": strings.ToLower(user)},
		"response_url": responseURL, "actions": []any{map[string]any{"action_id": action, "value": "shop-1.4.0"}},
	}
}

func TestPulsantiSlack(t *testing.T) {
	_, f, srv := setup(t)
	u, _ := url.Parse(srv.URL)
	dec := &fakeDecider{}
	h := &Interactions{
		SigningSecretRef: "signing", Secrets: mapSecrets{"signing": "s3cr3t", "bot": "xoxb-test"},
		Identities:    map[string]Identity{"U1": {Principal: "oidc:bob", Groups: []string{"cab"}}},
		EmailIdentity: "oidc:{email}",
		API:           &SlackAPI{URL: srv.URL + "/api", TokenRef: "bot", Secrets: mapSecrets{"bot": "xoxb-test"}, Client: srv.Client()},
		Decider:       dec, Language: "en", Client: srv.Client(), ResponseHosts: []string{u.Host},
		Logger: slog.New(slog.DiscardHandler),
	}
	now := time.Now()
	respond := srv.URL + "/respond"
	if code := signed(t, h, "s3cr3t", now, click("U1", ActionApprove, respond)); code != http.StatusOK {
		t.Fatalf("clic firmato: %d", code)
	}
	if code := signed(t, h, "s3cr3t", now, click("U3", ActionReject, respond)); code != http.StatusOK {
		t.Fatalf("clic firmato: %d", code)
	}
	if code := signed(t, h, "s3cr3t", now, click("U9", ActionApprove, respond)); code != http.StatusOK {
		t.Fatalf("clic firmato: %d", code)
	}
	// A wrong or expired signature is rejected without deciding anything.
	if code := signed(t, h, "altro", now, click("U1", ActionApprove, respond)); code != http.StatusUnauthorized {
		t.Errorf("firma errata: %d", code)
	}
	if code := signed(t, h, "s3cr3t", now.Add(-10*time.Minute), click("U1", ActionApprove, respond)); code != http.StatusUnauthorized {
		t.Errorf("richiesta scaduta: %d", code)
	}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.replies) == 3 })
	dec.mu.Lock()
	got := slices.Clone(dec.decisions)
	dec.mu.Unlock()
	slices.Sort(got)
	want := []string{"oidc:bob shop-1.4.0 true Slack @u1 [cab]", "oidc:dave@example.com shop-1.4.0 false Slack @u3 []"}
	if !slices.Equal(got, want) {
		t.Errorf("decisioni: %q", got)
	}
	f.mu.Lock()
	replies := strings.Join(f.replies, "\n")
	f.mu.Unlock()
	for _, want := range []string{"You approved shop-1.4.0.", "You rejected shop-1.4.0.", "Your Slack user (U9) is not linked"} {
		if !strings.Contains(replies, want) {
			t.Errorf("risposte senza %q: %s", want, replies)
		}
	}

	// A refusal by the service (e.g. self-approval) reaches whoever clicked.
	dec.mu.Lock()
	dec.err = errors.New("forbidden: oidc:bob ha richiesto il deploy")
	dec.mu.Unlock()
	signed(t, h, "s3cr3t", now, click("U1", ActionApprove, respond))
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.replies) == 4 && strings.Contains(f.replies[3], "not recorded: forbidden")
	})

	// A response_url to a host other than Slack receives nothing.
	if err := h.respond(context.Background(), "https://evil.example.com/x", "ciao"); err == nil {
		t.Error("response_url esterno accettato")
	}
}

// rawJSON serializes without escaping <, > and &, to compare Slack markup.
func rawJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return b.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condizione non raggiunta")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
