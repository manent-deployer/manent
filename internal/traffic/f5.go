package traffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Impostazioni del provider f5 (BIG-IP LTM, iControl REST).
const (
	settingManagementURL = "management_url" // URL di gestione del BIG-IP attivo
	settingPartition     = "partition"      // partizione del pool (default Common)
	settingPool          = "pool"           // pool LTM con i membri
	settingSyncGroup     = "sync_group"     // device group da sincronizzare dopo ogni modifica (config-sync)
)

var f5NameRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,255}$`)

// f5Pool svuota i membri di un pool LTM: session user-disabled lascia proseguire le
// connessioni esistenti (e quelle persistenti) e non ne accetta di nuove.
type f5Pool struct {
	base      string
	partition string
	pool      string
	sync      string
	creds     string
	client    *http.Client
}

func validateF5(s map[string]string) error {
	var errs []error
	if s[settingManagementURL] == "" {
		errs = append(errs, fmt.Errorf("%s obbligatorio", settingManagementURL))
	} else if err := checkURL(s, settingManagementURL, s[settingManagementURL]); err != nil {
		errs = append(errs, err)
	}
	if !f5NameRe.MatchString(s[settingPool]) {
		errs = append(errs, fmt.Errorf("%s obbligatorio", settingPool))
	}
	if p := s[settingPartition]; p != "" && !f5NameRe.MatchString(p) {
		errs = append(errs, fmt.Errorf("%s %q non valida", settingPartition, p))
	}
	if g := s[settingSyncGroup]; g != "" && !f5NameRe.MatchString(g) {
		errs = append(errs, fmt.Errorf("%s %q non valido", settingSyncGroup, g))
	}
	if s[settingCredentials] == "" {
		errs = append(errs, fmt.Errorf("%s obbligatorio (utente:password)", settingCredentials))
	}
	return errors.Join(errs...)
}

// NewF5Pool costruisce il pool f5.
func NewF5Pool(ctx context.Context, env Env) (Pool, error) {
	if err := validateF5(env.Settings); err != nil {
		return nil, err
	}
	c, err := httpClient(env.Settings, env.Timeout)
	if err != nil {
		return nil, err
	}
	p := &f5Pool{base: strings.TrimRight(env.Settings[settingManagementURL], "/"), partition: env.Settings[settingPartition],
		pool: env.Settings[settingPool], sync: env.Settings[settingSyncGroup], client: c}
	if p.partition == "" {
		p.partition = "Common"
	}
	if p.creds, err = env.Secrets.Secret(ctx, env.Settings[settingCredentials]); err != nil {
		return nil, fmt.Errorf("%s: %w", settingCredentials, err)
	}
	return p, nil
}

func (p *f5Pool) ref(name string) string { return "~" + p.partition + "~" + url.PathEscape(name) }

func (p *f5Pool) membersURL() string {
	return p.base + "/mgmt/tm/ltm/pool/" + p.ref(p.pool) + "/members"
}

type f5Member struct {
	Name    string `json:"name"`
	Session string `json:"session"`
	State   string `json:"state"`
}

func (p *f5Pool) do(ctx context.Context, method, u string, body any, out any) error {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return doJSON(ctx, p.client, method, u, raw, out, func(r *http.Request) {
		if user, pass, ok := strings.Cut(p.creds, ":"); ok {
			r.SetBasicAuth(user, strings.TrimSpace(pass))
		}
	})
}

func (p *f5Pool) list(ctx context.Context) ([]f5Member, error) {
	var out struct {
		Items []f5Member `json:"items"`
	}
	if err := p.do(ctx, http.MethodGet, p.membersURL(), nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (m f5Member) inService() bool {
	return m.State == "up" && m.Session != "user-disabled"
}

// Members implementa Pool.
func (p *f5Pool) Members(ctx context.Context) ([]Member, error) {
	list, err := p.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(list))
	for _, m := range list {
		st := StateActive
		switch {
		case m.State == "user-down":
			st = StateDisabled
		case m.Session == "user-disabled":
			st = StateDraining
		}
		out = append(out, Member{Name: m.Name, State: st, Healthy: m.State == "up"})
	}
	return out, nil
}

func (p *f5Pool) patch(ctx context.Context, members []string, body map[string]string) error {
	for _, m := range members {
		if err := p.do(ctx, http.MethodPatch, p.membersURL()+"/"+p.ref(m), body, nil); err != nil {
			return fmt.Errorf("membro %s: %w", m, err)
		}
	}
	return p.configSync(ctx)
}

// configSync propaga la modifica agli altri dispositivi del device group.
func (p *f5Pool) configSync(ctx context.Context) error {
	if p.sync == "" {
		return nil
	}
	return p.do(ctx, http.MethodPost, p.base+"/mgmt/tm/cm", map[string]string{"command": "run", "utilCmdArgs": "config-sync to-group " + p.sync}, nil)
}

// Drain implementa Pool.
func (p *f5Pool) Drain(ctx context.Context, members []string) error {
	return p.patch(ctx, members, map[string]string{"session": "user-disabled"})
}

// Restore implementa Pool.
func (p *f5Pool) Restore(ctx context.Context, members []string) error {
	return p.patch(ctx, members, map[string]string{"session": "user-enabled", "state": "user-up"})
}

// Connections implementa Pool: le connessioni lato server (serverside.curConns) del membro.
func (p *f5Pool) Connections(ctx context.Context, members []string) (map[string]int, error) {
	out := map[string]int{}
	for _, m := range members {
		var stats map[string]any
		if err := p.do(ctx, http.MethodGet, p.membersURL()+"/"+p.ref(m)+"/stats", nil, &stats); err != nil {
			return nil, fmt.Errorf("statistiche del membro %s: %w", m, err)
		}
		n, ok := findStat(stats, "serverside.curConns")
		if !ok {
			return nil, fmt.Errorf("statistiche del membro %s senza serverside.curConns", m)
		}
		out[m] = n
	}
	return out, nil
}

// findStat cerca ricorsivamente un contatore nelle statistiche annidate di iControl REST.
func findStat(v any, key string) (int, bool) {
	switch x := v.(type) {
	case map[string]any:
		if e, ok := x[key].(map[string]any); ok {
			if n, ok := e["value"].(float64); ok {
				return int(n), true
			}
		}
		for _, k := range sortedKeys(x) {
			if n, ok := findStat(x[k], key); ok {
				return n, true
			}
		}
	case []any:
		for _, e := range x {
			if n, ok := findStat(e, key); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Healthy implementa Pool.
func (p *f5Pool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	list, err := p.list(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range members {
		i := slices.IndexFunc(list, func(x f5Member) bool { return x.Name == m })
		out[m] = i >= 0 && list[i].inService()
	}
	return out, nil
}
