package traffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/manent-deployer/manent/internal/descriptor"
)

// Routers on traditional load balancers: the slots are the servers with the slot label of the
// same backend (or pool, or upstream), and a slot's traffic share is obtained with the
// weights of its servers. A weight of 0 sends no new requests to the server.

// slotMembers groups the target members by slot.
func slotMembers(env Env) (map[string][]string, error) {
	out := map[string][]string{}
	for _, t := range env.Targets {
		s := t.Labels[descriptor.SlotLabel]
		if !descriptor.ValidSlot(s) {
			return nil, fmt.Errorf("target %s without label %s (a or b)", t.Host, descriptor.SlotLabel)
		}
		out[s] = append(out[s], env.Member(t))
	}
	return out, nil
}

// memberWeights turns slot shares into server weights: the sum of the weights of a slot's
// servers is proportional to its share. maxWeight is the load balancer's maximum weight.
func memberWeights(shares map[string]int, members map[string][]string, maxWeight int) map[string]int {
	raw := map[string]float64{}
	top := 0.0
	for slot, ms := range members {
		if len(ms) == 0 {
			continue
		}
		w := float64(shares[slot]) / float64(len(ms))
		for _, m := range ms {
			raw[m] = w
			top = math.Max(top, w)
		}
	}
	out := map[string]int{}
	for m, w := range raw {
		switch {
		case w <= 0:
			out[m] = 0
		default:
			out[m] = max(1, int(math.Round(w*float64(maxWeight)/top)))
		}
	}
	return out
}

// slotShares derives the slot shares from the server weights.
func slotShares(weights map[string]int, members map[string][]string) map[string]int {
	sums, total := map[string]int{}, 0
	for slot, ms := range members {
		for _, m := range ms {
			sums[slot] += weights[m]
			total += weights[m]
		}
	}
	out := map[string]int{}
	for _, slot := range descriptor.Slots {
		if total > 0 {
			out[slot] = int(math.Round(float64(sums[slot]) * 100 / float64(total)))
		}
	}
	return out
}

// memberRouter is a Router that reads and sets the weights of individual servers.
type memberRouter struct {
	members   map[string][]string
	maxWeight int
	get       func(ctx context.Context, member string) (int, error)
	set       func(ctx context.Context, member string, weight int) error
}

func (r *memberRouter) Weights(ctx context.Context) (map[string]int, error) {
	weights := map[string]int{}
	for _, ms := range r.members {
		for _, m := range ms {
			w, err := r.get(ctx, m)
			if err != nil {
				return nil, fmt.Errorf("weight of server %s: %w", m, err)
			}
			weights[m] = w
		}
	}
	return slotShares(weights, r.members), nil
}

func (r *memberRouter) SetWeights(ctx context.Context, shares map[string]int) error {
	for m, w := range memberWeights(shares, r.members, r.maxWeight) {
		if cur, err := r.get(ctx, m); err == nil && cur == w {
			continue
		}
		if err := r.set(ctx, m, w); err != nil {
			return fmt.Errorf("weight of server %s: %w", m, err)
		}
	}
	return nil
}

// --- HAProxy: server weights through the configuration API of the Data Plane API ---

// NewHAProxyRouter builds the haproxy router: the weights (0-256) of the backend servers, on
// every instance, through the configuration API (applied at runtime when possible).
func NewHAProxyRouter(ctx context.Context, env Env) (Router, error) {
	pool, err := NewHAProxyPool(ctx, env)
	if err != nil {
		return nil, err
	}
	hp := pool.(*haproxyPool)
	members, err := slotMembers(env)
	if err != nil {
		return nil, err
	}
	serverURL := func(base, m string) string {
		return base + "/" + hp.version + "/services/haproxy/configuration/backends/" + url.PathEscape(hp.backend) + "/servers/" + url.PathEscape(m)
	}
	return &memberRouter{members: members, maxWeight: 256,
		get: func(ctx context.Context, m string) (int, error) {
			var srv map[string]any
			if err := hp.do(ctx, http.MethodGet, serverURL(hp.urls[0], m), nil, &srv); err != nil {
				return 0, err
			}
			if w, ok := srv["weight"].(float64); ok {
				return int(w), nil
			}
			return 1, nil
		},
		set: func(ctx context.Context, m string, w int) error {
			for _, base := range hp.urls {
				var ver int
				if err := hp.do(ctx, http.MethodGet, base+"/"+hp.version+"/services/haproxy/configuration/version", nil, &ver); err != nil {
					return err
				}
				var srv map[string]any
				if err := hp.do(ctx, http.MethodGet, serverURL(base, m), nil, &srv); err != nil {
					return err
				}
				srv["weight"] = w
				body, _ := json.Marshal(srv)
				if err := hp.do(ctx, http.MethodPut, serverURL(base, m)+"?version="+strconv.Itoa(ver), body, nil); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}

// --- F5 BIG-IP: ratio of the pool members (ratio-member load balancing) ---

// NewF5Router builds the f5 router: the ratio (1-100) of the pool members, which must use a
// ratio load balancing method; a weight of 0 disables the member (session user-disabled).
func NewF5Router(ctx context.Context, env Env) (Router, error) {
	pool, err := NewF5Pool(ctx, env)
	if err != nil {
		return nil, err
	}
	fp := pool.(*f5Pool)
	members, err := slotMembers(env)
	if err != nil {
		return nil, err
	}
	return &memberRouter{members: members, maxWeight: 100,
		get: func(ctx context.Context, m string) (int, error) {
			var mem map[string]any
			if err := fp.do(ctx, http.MethodGet, fp.membersURL()+"/"+fp.ref(m), nil, &mem); err != nil {
				return 0, err
			}
			if mem["session"] == "user-disabled" {
				return 0, nil
			}
			if r, ok := mem["ratio"].(float64); ok {
				return int(r), nil
			}
			return 1, nil
		},
		set: func(ctx context.Context, m string, w int) error {
			body := map[string]any{"session": "user-disabled"}
			if w > 0 {
				body = map[string]any{"session": "user-enabled", "ratio": w}
			}
			if err := fp.do(ctx, http.MethodPatch, fp.membersURL()+"/"+fp.ref(m), body, nil); err != nil {
				return err
			}
			return fp.configSync(ctx)
		},
	}, nil
}

// --- NGINX Plus: weight of the upstream servers ---

// NewNginxPlusRouter builds the nginx-plus router: the weight of the upstream servers on
// every instance; a weight of 0 puts the server in drain.
func NewNginxPlusRouter(ctx context.Context, env Env) (Router, error) {
	pool, err := NewNginxPlusPool(ctx, env)
	if err != nil {
		return nil, err
	}
	np := pool.(*nginxPlusPool)
	members, err := slotMembers(env)
	if err != nil {
		return nil, err
	}
	find := func(ctx context.Context, base, m string) (*nginxServer, error) {
		var servers []nginxServer
		if err := np.do(ctx, http.MethodGet, np.upstreamURL(base)+"/servers", nil, &servers); err != nil {
			return nil, err
		}
		for i := range servers {
			if servers[i].Server == m {
				return &servers[i], nil
			}
		}
		return nil, errors.New("server missing from upstream " + np.upstream)
	}
	return &memberRouter{members: members, maxWeight: 100,
		get: func(ctx context.Context, m string) (int, error) {
			s, err := find(ctx, np.urls[0], m)
			if err != nil {
				return 0, err
			}
			if s.Drain || s.Down {
				return 0, nil
			}
			return max(s.Weight, 1), nil
		},
		set: func(ctx context.Context, m string, w int) error {
			body := []byte(`{"drain":true}`)
			if w > 0 {
				body = []byte(`{"drain":false,"down":false,"weight":` + strconv.Itoa(w) + `}`)
			}
			for _, base := range np.urls {
				s, err := find(ctx, base, m)
				if err != nil {
					return err
				}
				if err := np.do(ctx, http.MethodPatch, np.upstreamURL(base)+"/servers/"+strconv.Itoa(s.ID), body, nil); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}
