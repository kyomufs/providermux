package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Rotator asks a running proxy controller for a different exit address.
//
// WHY THE CONTROLLER AND NOT THE CONFIG. The proxy's node list belongs to
// whoever maintains the subscription: several hundred entries, renamed and
// reshuffled whenever the list is refreshed. A copy of it in this daemon's
// configuration would be stale the day it was written, and editing the proxy's
// own file from here would fight the process that owns it. The controller API
// answers the only two questions that matter — which nodes exist and whether
// one of them carries traffic — without either side knowing the other's layout.
//
// WHY A LIVENESS PROBE. The list contains dead nodes; that is measured, not
// assumed (one of the first nodes tried answered a probe with a timeout). A
// rotation that trusts the list trades a quota refusal for a connect timeout,
// which is a worse answer for the same money: the client's turn still fails,
// and the failure takes longer to arrive.
type Rotator struct {
	controller *url.URL
	group      string
	secret     string
	probeURL   string
	probeCount int
	client     *http.Client

	mu     sync.Mutex
	cursor int
}

// RotatorConfig are the settings of the rotator.
type RotatorConfig struct {
	// Controller is the base URL of the proxy's controller.
	Controller string
	// Group is the selector group whose current choice is the exit.
	Group string
	// Secret is the controller's shared secret, if it has one.
	Secret string
	// ProbeURL is fetched through a candidate node to prove it carries traffic.
	ProbeURL string
	// ProbeCount bounds how many candidates one rotation tries.
	ProbeCount int
}

// Budgets. A rotation happens inside a client request, so every number here is
// a delay the client also waits for; they are short for that reason.
const (
	controllerTimeout = 5 * time.Second
	probeTimeout      = 4 * time.Second
)

// NewRotator creates the rotator.
func NewRotator(cfg RotatorConfig) (*Rotator, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.Controller))
	if err != nil {
		return nil, fmt.Errorf("egress: invalid controller %q: %w", cfg.Controller, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("egress: controller %q: want an http(s) URL", cfg.Controller)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("egress: controller %q has no host", cfg.Controller)
	}
	if cfg.Group == "" {
		return nil, fmt.Errorf("egress: rotation group must not be empty")
	}
	if cfg.ProbeCount < 1 {
		return nil, fmt.Errorf("egress: probe count must be positive")
	}
	probe := cfg.ProbeURL
	if probe == "" {
		probe = DefaultURL
	}
	return &Rotator{
		controller: u,
		group:      cfg.Group,
		secret:     cfg.Secret,
		probeURL:   probe,
		probeCount: cfg.ProbeCount,
		client:     &http.Client{},
	}, nil
}

// Rotate moves the group to a different, live node and reports its name.
//
// Candidates are taken in the controller's own order, starting after the
// current choice, so consecutive rotations walk the list instead of probing the
// same few nodes. The node is probed before it is selected: the group is left
// untouched when nothing alive is found, because a dead exit is worse than the
// spent one the caller already had.
func (r *Rotator) Rotate(ctx context.Context) (string, error) {
	all, now, err := r.list(ctx)
	if err != nil {
		return "", err
	}
	if len(all) < 2 {
		return "", fmt.Errorf("egress: group %q has %d node(s): nothing to rotate to", r.group, len(all))
	}

	start := 0
	for i, name := range all {
		if name == now {
			start = i + 1
			break
		}
	}

	tried := 0
	for offset := 0; offset < len(all) && tried < r.probeCount; offset++ {
		name := all[(start+offset)%len(all)]
		if name == now {
			continue
		}
		tried++
		if err := r.probe(ctx, name); err != nil {
			continue
		}
		if err := r.selectNode(ctx, name); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("egress: no live node among %d candidate(s) of group %q", tried, r.group)
}

// list returns the group's nodes and its current choice.
func (r *Rotator) list(ctx context.Context) ([]string, string, error) {
	var body struct {
		All []string `json:"all"`
		Now string   `json:"now"`
	}
	if err := r.get(ctx, r.group, &body, controllerTimeout, ""); err != nil {
		return nil, "", fmt.Errorf("egress: reading group %q: %w", r.group, err)
	}
	return body.All, body.Now, nil
}

// probe asks the controller to fetch probeURL through the node.
//
// The controller performs the request on the node's side, so a success says
// both that the node is reachable and that it carries traffic. A node that
// answers with a delay of zero is treated as dead: the field is the round trip
// the controller measured, and zero means it measured nothing.
func (r *Rotator) probe(ctx context.Context, name string) error {
	var body struct {
		Delay int `json:"delay"`
	}
	if err := r.get(ctx, name+"/delay", &body, probeTimeout, r.probeURL); err != nil {
		return err
	}
	if body.Delay <= 0 {
		return fmt.Errorf("egress: node %q reported no delay", name)
	}
	return nil
}

// selectNode makes the group's choice the given node.
func (r *Rotator) selectNode(ctx context.Context, name string) error {
	payload, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, controllerTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPut, r.url(r.group, ""), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	r.authorize(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("egress: selecting node %q: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("egress: selecting node %q: %s: %s", name, resp.Status, excerpt(detail))
	}
	return nil
}

// get reads a controller resource into out.
func (r *Rotator) get(ctx context.Context, path string, out any, timeout time.Duration, probe string) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, r.url(path, probe), nil)
	if err != nil {
		return err
	}
	r.authorize(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, excerpt(detail))
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decoding the controller's answer: %w", err)
	}
	return nil
}

// url builds a controller address. The node name is one path segment even when
// it contains spaces or punctuation: those are recorded as they are displayed.
//
// The name goes into Path unescaped and the URL escapes it on the way out.
// Escaping it here would escape the escapes ("%D0…" → "%25D0…") and the
// controller would then look up a node that does not exist.
func (r *Rotator) url(path, probe string) string {
	u := *r.controller
	u.Path = strings.TrimSuffix(u.Path, "/") + "/proxies/" + path
	if probe != "" {
		q := url.Values{}
		q.Set("timeout", fmt.Sprint(probeTimeout.Milliseconds()))
		q.Set("url", probe)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// authorize attaches the controller's secret.
func (r *Rotator) authorize(req *http.Request) {
	if r.secret != "" {
		req.Header.Set("authorization", "Bearer "+r.secret)
	}
}

// excerpt renders at most n bytes of a body as a single log-safe line.
func excerpt(body []byte) string {
	s := strings.TrimSpace(strings.Join(strings.Fields(string(body)), " "))
	if s == "" {
		return "(empty)"
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
