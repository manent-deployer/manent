package change

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/secrets"
)

// ServiceNow uses the Table API on the change request table. The deploy's ticket is the one
// whose correlation_id equals the deploy ID.
type ServiceNow struct {
	URL      string
	Username string
	// PasswordRef (with Username) or TokenRef: secret references, read at every use.
	PasswordRef string
	TokenRef    string
	Table       string
	Fields      map[string]string
	// StartState is the state set by Start ("" = no change).
	StartState string
	// CloseStates are the states set in order by Close.
	CloseStates []string
	// CloseCodes per outcome: "success" and "failure".
	CloseCodes map[string]string
	Secrets    secrets.Provider
	Client     HTTPDoer
}

// Default values of the standard ServiceNow state model.
const (
	SNStateImplement = "-1"
	SNStateReview    = "0"
	SNStateClosed    = "3"
	SNStateCanceled  = "4"
)

// Name returns the system name.
func (s *ServiceNow) Name() string { return "servicenow" }

func (s *ServiceNow) table() string {
	if s.Table != "" {
		return s.Table
	}
	return "change_request"
}

func (s *ServiceNow) base() string {
	return strings.TrimRight(s.URL, "/") + "/api/now/table/" + url.PathEscape(s.table())
}

func (s *ServiceNow) auth(ctx context.Context) (func(*http.Request), error) {
	c := credentials{Secrets: s.Secrets}
	if s.TokenRef != "" {
		tok, err := c.resolve(ctx, s.TokenRef)
		if err != nil {
			return nil, err
		}
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }, nil
	}
	pw, err := c.resolve(ctx, s.PasswordRef)
	if err != nil {
		return nil, err
	}
	return func(r *http.Request) { r.SetBasicAuth(s.Username, pw) }, nil
}

// snRecord is a Table API record (sysparm_display_value=false: raw values).
type snRecord struct {
	SysID    string `json:"sys_id"`
	Number   string `json:"number"`
	State    string `json:"state"`
	Approval string `json:"approval"`
}

func (s *ServiceNow) ticket(r snRecord) Ticket {
	t := Ticket{ID: r.SysID, Number: r.Number, State: r.State, Approval: ApprovalPending}
	t.URL = strings.TrimRight(s.URL, "/") + "/nav_to.do?uri=" + url.QueryEscape(s.table()+".do?sys_id="+r.SysID)
	switch {
	case r.Approval == "approved":
		t.Approval = ApprovalApproved
	case r.Approval == "rejected", r.State == SNStateCanceled:
		t.Approval = ApprovalRejected
	}
	return t
}

const snFields = "sys_id,number,state,approval"

// Open looks for the change whose correlation_id equals the deploy ID and creates it if missing.
func (s *ServiceNow) Open(ctx context.Context, r Request) (Ticket, error) {
	auth, err := s.auth(ctx)
	if err != nil {
		return Ticket{}, err
	}
	q := url.Values{
		"sysparm_query":  {"correlation_id=" + r.DeploymentID + "^ORDERBYDESCsys_created_on"},
		"sysparm_fields": {snFields}, "sysparm_limit": {"1"},
	}
	var found struct {
		Result []snRecord `json:"result"`
	}
	if _, err := doJSON(ctx, s.Client, http.MethodGet, s.base()+"?"+q.Encode(), auth, nil, &found); err != nil {
		return Ticket{}, fmt.Errorf("searching the change: %w", err)
	}
	if len(found.Result) > 0 {
		return s.ticket(found.Result[0]), nil
	}
	body := map[string]any{
		"short_description":   summary(r),
		"description":         description(r),
		"correlation_id":      r.DeploymentID,
		"correlation_display": "manent",
	}
	for _, k := range slices.Sorted(maps.Keys(s.Fields)) {
		body[k] = expand(s.Fields[k], r)
	}
	var created struct {
		Result snRecord `json:"result"`
	}
	if _, err := doJSON(ctx, s.Client, http.MethodPost, s.base()+"?sysparm_fields="+snFields, auth, body, &created); err != nil {
		return Ticket{}, fmt.Errorf("creating the change: %w", err)
	}
	return s.ticket(created.Result), nil
}

// Refresh reads state and approval again.
func (s *ServiceNow) Refresh(ctx context.Context, t Ticket) (Ticket, error) {
	auth, err := s.auth(ctx)
	if err != nil {
		return t, err
	}
	var got struct {
		Result snRecord `json:"result"`
	}
	if _, err := doJSON(ctx, s.Client, http.MethodGet, s.base()+"/"+url.PathEscape(t.ID)+"?sysparm_fields="+snFields, auth, nil, &got); err != nil {
		return t, fmt.Errorf("reading change %s: %w", t.Number, err)
	}
	return s.ticket(got.Result), nil
}

func (s *ServiceNow) patch(ctx context.Context, t Ticket, body map[string]any) error {
	auth, err := s.auth(ctx)
	if err != nil {
		return err
	}
	_, err = doJSON(ctx, s.Client, http.MethodPatch, s.base()+"/"+url.PathEscape(t.ID)+"?sysparm_fields="+snFields, auth, body, nil)
	return err
}

// Start moves the change to the implementation state.
func (s *ServiceNow) Start(ctx context.Context, t Ticket) error {
	if s.StartState == "" || t.State == s.StartState {
		return nil
	}
	if err := s.patch(ctx, t, map[string]any{"state": s.StartState, "work_notes": "Deployment started by manent."}); err != nil {
		return fmt.Errorf("starting change %s: %w", t.Number, err)
	}
	return nil
}

// Close records the outcome (close_code, close_notes) and moves the change through the
// closing states, in order. A state already reached is not set again, so closing can be repeated.
func (s *ServiceNow) Close(ctx context.Context, t Ticket, o Outcome) error {
	code := s.CloseCodes["success"]
	if !o.Success {
		code = s.CloseCodes["failure"]
	}
	notes := closeNotes(o)
	cur, err := s.Refresh(ctx, t)
	if err != nil {
		return err
	}
	if len(s.CloseStates) == 0 {
		return s.patch(ctx, t, map[string]any{"close_code": code, "close_notes": notes, "work_notes": notes})
	}
	reached := slices.Index(s.CloseStates, cur.State)
	for i, st := range s.CloseStates {
		if i <= reached {
			continue
		}
		body := map[string]any{"state": st, "close_code": code, "close_notes": notes}
		if i == reached+1 {
			body["work_notes"] = notes
		}
		if err := s.patch(ctx, t, body); err != nil {
			return fmt.Errorf("closing change %s (state %s): %w", t.Number, st, err)
		}
	}
	return nil
}
