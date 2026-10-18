package chat

import (
	"fmt"
	"strings"
	"time"
)

// text holds the message phrases in one language.
type text struct {
	lang                                                       string
	started, succeeded, failed, dryRun                         string
	approvalRequested, approvalApproved                        string
	approvalRejected, approvalExpired                          string
	application, environment, version, requestedBy, deployment string
	tenant                                                     string
	kind, kinds                                                map[string]string
	errorLabel, rollback, approvals, change, changeApproval    string
	expires, open, approve, reject, approveIn                  string
	decisionApprove, decisionReject                            string
}

var languages = map[string]text{
	"en": {
		lang:    "en",
		started: "Deploy started", succeeded: "Deploy succeeded", failed: "Deploy failed", dryRun: "dry-run",
		approvalRequested: "Approval requested", approvalApproved: "Approved", approvalRejected: "Rejected", approvalExpired: "Approval expired",
		application: "Application", environment: "Environment", version: "Version", requestedBy: "Requested by", deployment: "Deployment", tenant: "Tenant",
		kinds:      map[string]string{"promotion": "promotion", "rollback": "rollback"},
		errorLabel: "Error", rollback: "Automatic rollback", approvals: "Approvals", change: "Change", changeApproval: "change approval in ITSM",
		expires: "Expires", open: "Open in manent", approve: "Approve", reject: "Reject", approveIn: "Review in manent",
		decisionApprove: "approved", decisionReject: "rejected",
	},
	"it": {
		lang:    "it",
		started: "Deploy avviato", succeeded: "Deploy riuscito", failed: "Deploy fallito", dryRun: "dry-run",
		approvalRequested: "Approvazione richiesta", approvalApproved: "Approvato", approvalRejected: "Respinto", approvalExpired: "Approvazione scaduta",
		application: "Applicazione", environment: "Ambiente", version: "Versione", requestedBy: "Richiesto da", deployment: "Deploy", tenant: "Tenant",
		kinds:      map[string]string{"promotion": "promozione", "rollback": "rollback"},
		errorLabel: "Errore", rollback: "Rollback automatico", approvals: "Approvazioni", change: "Change", changeApproval: "approvazione del change nell'ITSM",
		expires: "Scade", open: "Apri in manent", approve: "Approva", reject: "Respingi", approveIn: "Valuta in manent",
		decisionApprove: "ha approvato", decisionReject: "ha respinto",
	},
}

func texts(lang string) text {
	if t, ok := languages[lang]; ok {
		return t
	}
	return languages["en"]
}

// title is the event title.
func (t text) title(event string) string {
	switch event {
	case EventStarted:
		return t.started
	case EventSucceeded:
		return t.succeeded
	case EventFailed:
		return t.failed
	case EventApprovalRequested:
		return t.approvalRequested
	case EventApprovalApproved:
		return t.approvalApproved
	case EventApprovalRejected:
		return t.approvalRejected
	case EventApprovalExpired:
		return t.approvalExpired
	}
	return event
}

// emoji is the outcome sign in Slack messages.
func emoji(event string) string {
	switch event {
	case EventSucceeded, EventApprovalApproved:
		return ":white_check_mark:"
	case EventFailed, EventApprovalRejected:
		return ":x:"
	case EventApprovalExpired:
		return ":hourglass:"
	case EventApprovalRequested:
		return ":raised_hand:"
	}
	return ":rocket:"
}

// headline is the summary line, used as notification text.
func headline(t text, m Message) string {
	s := fmt.Sprintf("%s: %s %s → %s", t.title(m.Event), m.Application, m.Version, m.Environment)
	if m.Tenant != "" {
		s += " / " + m.Tenant
	}
	if k := t.kinds[m.Kind]; k != "" {
		s += " (" + k + ")"
	}
	if m.DryRun {
		s += " [" + t.dryRun + "]"
	}
	return s
}

// facts are the label/value pairs of the message, in order.
func facts(t text, m Message) [][2]string {
	f := [][2]string{
		{t.application, m.Application}, {t.environment, m.Environment},
		{t.version, strings.TrimSpace(m.Artifact + " " + m.Version)},
	}
	if m.Tenant != "" {
		f = append(f[:2], append([][2]string{{t.tenant, m.Tenant}}, f[2:]...)...)
	}
	if m.RequestedBy != "" {
		f = append(f, [2]string{t.requestedBy, m.RequestedBy})
	}
	f = append(f, [2]string{t.deployment, m.DeploymentID})
	if m.Change != nil && m.Change.Number != "" {
		f = append(f, [2]string{t.change, m.Change.Number})
	}
	if a := m.Approval; a != nil {
		var parts []string
		if a.Required > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d", approvedCount(a), a.Required))
		}
		if a.Change {
			parts = append(parts, t.changeApproval)
		}
		if len(parts) > 0 {
			f = append(f, [2]string{t.approvals, strings.Join(parts, " + ")})
		}
		if m.Event == EventApprovalRequested && !a.ExpiresAt.IsZero() {
			f = append(f, [2]string{t.expires, a.ExpiresAt.UTC().Format(time.RFC1123)})
		}
	}
	if m.Error != "" {
		f = append(f, [2]string{t.errorLabel, truncate(m.Error, 500)})
	}
	if m.Rollback != "" {
		f = append(f, [2]string{t.rollback, m.Rollback})
	}
	return f
}

func approvedCount(a *Approval) int {
	n := 0
	for _, d := range a.Decisions {
		if d.Approve {
			n++
		}
	}
	return n
}

// decisions describes the decisions already taken.
func decisions(t text, a *Approval) []string {
	if a == nil {
		return nil
	}
	var out []string
	for _, d := range a.Decisions {
		verb := t.decisionApprove
		if !d.Approve {
			verb = t.decisionReject
		}
		s := d.Approver + " " + verb
		if d.Comment != "" {
			s += ": " + truncate(d.Comment, 200)
		}
		out = append(out, s)
	}
	return out
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// mrkdwnEscape escapes the special characters of Slack markup.
func mrkdwnEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Actions of the approval buttons in Slack messages.
const (
	ActionApprove = "manent_approve"
	ActionReject  = "manent_reject"
)

// slackBlocks builds the message in Block Kit. With interactive, an approval request has the
// Approve and Reject buttons (it needs the app with the bot and the interactions URL
// configured); the link to manent is always there, if known.
func slackBlocks(t text, m Message, link string, interactive bool) []any {
	title := emoji(m.Event) + " *" + mrkdwnEscape(headline(t, m)) + "*"
	var fields []any
	for _, f := range facts(t, m) {
		v := mrkdwnEscape(f[1])
		if f[0] == t.change && m.Change != nil && m.Change.URL != "" {
			v = "<" + m.Change.URL + "|" + v + ">"
		}
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": "*" + mrkdwnEscape(f[0]) + "*\n" + v})
	}
	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": title}}}
	for i := 0; i < len(fields); i += 10 {
		// Slack allows at most 10 fields per section.
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields[i:min(i+10, len(fields))]})
	}
	if ds := decisions(t, m.Approval); len(ds) > 0 {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{
			map[string]any{"type": "mrkdwn", "text": mrkdwnEscape(strings.Join(ds, "\n"))},
		}})
	}
	var actions []any
	if interactive && m.Event == EventApprovalRequested && m.Approval != nil && m.Approval.Required > 0 {
		actions = append(actions,
			map[string]any{"type": "button", "action_id": ActionApprove, "style": "primary", "value": m.DeploymentID,
				"text": map[string]any{"type": "plain_text", "text": t.approve}},
			map[string]any{"type": "button", "action_id": ActionReject, "style": "danger", "value": m.DeploymentID,
				"text": map[string]any{"type": "plain_text", "text": t.reject}},
		)
	}
	if link != "" {
		label := t.open
		if m.Event == EventApprovalRequested {
			label = t.approveIn
		}
		actions = append(actions, map[string]any{"type": "button", "action_id": "manent_open", "url": link,
			"text": map[string]any{"type": "plain_text", "text": label}})
	}
	if len(actions) > 0 {
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "manent_actions", "elements": actions})
	}
	return blocks
}

// teamsCard builds the message as an Adaptive Card, in the format accepted by Teams Workflows
// webhooks. Approvals are decided from the web interface.
func teamsCard(t text, m Message, link string) map[string]any {
	color := "Default"
	switch m.Event {
	case EventSucceeded, EventApprovalApproved:
		color = "Good"
	case EventFailed, EventApprovalRejected:
		color = "Attention"
	case EventApprovalRequested, EventApprovalExpired:
		color = "Warning"
	}
	var fs []any
	for _, f := range facts(t, m) {
		fs = append(fs, map[string]any{"title": f[0], "value": f[1]})
	}
	body := []any{
		map[string]any{"type": "TextBlock", "text": headline(t, m), "weight": "Bolder", "size": "Medium", "wrap": true, "color": color},
		map[string]any{"type": "FactSet", "facts": fs},
	}
	if ds := decisions(t, m.Approval); len(ds) > 0 {
		body = append(body, map[string]any{"type": "TextBlock", "text": strings.Join(ds, "\n\n"), "wrap": true, "isSubtle": true})
	}
	var actions []any
	if link != "" {
		label := t.open
		if m.Event == EventApprovalRequested {
			label = t.approveIn
		}
		actions = append(actions, map[string]any{"type": "Action.OpenUrl", "title": label, "url": link})
	}
	if m.Change != nil && m.Change.URL != "" {
		actions = append(actions, map[string]any{"type": "Action.OpenUrl", "title": t.change + " " + m.Change.Number, "url": m.Change.URL})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard", "version": "1.4", "body": body,
	}
	if len(actions) > 0 {
		card["actions"] = actions
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil, "content": card,
		}},
	}
}
