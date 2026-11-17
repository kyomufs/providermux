// Command laneprobe measures what the free lane counts.
//
// A refused request is counted against more than one thing, and the counters
// have different windows (lane.go in internal/provider/opencode records the
// measurement). Which counter dominates decides what a client can do about a
// refusal: if the allowance belongs to the address, a new session buys nothing
// and the address has to change; if it also belongs to the session identifier,
// changing the address is not enough while the identifier stays.
//
// The probe answers that with the smallest number of requests it can:
//
//	cell 0      a fresh identifier on the current address, refused and thereby
//	            spent — this identifier becomes S0
//	cell i      a NEW address. First a single request carrying S0 — the
//	            identifier that was spent on the first address. Then a fresh
//	            identifier, spent in turn.
//
// Reading the result:
//
//	S0 accepted on a fresh address, fresh identifier accepted too
//	    the session counter does not travel: a new address alone recovers the
//	    conversation, and the identifier may be kept.
//	S0 refused on a fresh address while a fresh identifier is accepted
//	    the session counter travels with the identifier: a spent conversation
//	    must leave under a new name once the address changes. That is the
//	    rotation implemented in lane.go.
//	S0 refused and the fresh identifier refused too
//	    the address itself is spent — or the probe has found an address the
//	    lane already knew. Neither is fixable from the client side.
//
// It sends real requests against a real allowance, so the defaults are small.
// Run it only when the addresses it visits can be spared.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// canonical session identifiers: ses_, twelve hex digits, fourteen base62. The
// shape the service accepts is stated in sessionids.go; the probe mints one by
// hand so that it controls identity itself and the provider does not rewrite it.
const sessionAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// cell is one address and what happened on it.
type cell struct {
	Address string `json:"address"`
	// Replay is the status of the request carrying S0, zero when the cell is the
	// one that spent S0.
	Replay int `json:"replay_status"`
	// Ok is how many requests the fresh identifier got through.
	Ok int `json:"fresh_ok"`
	// Status is the status that stopped the fresh identifier, zero if the
	// allowance outlived the probe's budget.
	Status int `json:"fresh_status"`
	// RetryAfter is the wait the refusal named, in seconds, zero when absent.
	RetryAfter int `json:"retry_after"`
	// Session is the identifier the cell's fresh burst carried.
	Session string `json:"session"`
}

func main() {
	var (
		base      = flag.String("base", "http://127.0.0.1:8787", "gateway address")
		model     = flag.String("model", "opencode/deepseek-v4.1-flash-free", "provider/model, as the gateway addresses it")
		cells     = flag.Int("cells", 4, "number of addresses to measure, including the current one")
		budget    = flag.Int("max", 4, "requests per fresh identifier before the probe gives up on that cell")
		switchCmd = flag.String("switch-cmd", "", "shell command that changes the exit address; run before every cell but the first")
		addrURL   = flag.String("address-url", "https://cloudflare.com/cdn-cgi/trace", "echo service that reports the exit address")
		wait      = flag.Duration("switch-wait", 90*time.Second, "how long to wait for the exit address to change")
		prompt    = flag.String("prompt", "Reply with the single word: ok", "user message the probe sends")
		out       = flag.String("out", "", "write the measurements to this JSON file")
		jsonOnly  = flag.Bool("json", false, "print only the JSON")
	)
	flag.Parse()

	p := &probe{
		base:    strings.TrimRight(*base, "/"),
		model:   *model,
		prompt:  *prompt,
		client:  &http.Client{Timeout: 180 * time.Second},
		addrURL: *addrURL,
	}
	logf := func(format string, args ...any) {
		if !*jsonOnly {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}

	cells0 := make([]cell, 0, *cells)

	address, err := p.address()
	if err != nil {
		fatal("resolving the exit address: %v", err)
	}

	// Cell 0 spends the identifier that the other cells will replay.
	s0 := newSession()
	spent, refused := p.burst(s0, *budget)
	cells0 = append(cells0, cell{Address: address, Ok: spent, Status: refused.status, RetryAfter: refused.retryAfter, Session: s0})
	logf("cell 1 %s: fresh identifier got %d through, then %s", address, spent, describe(refused.status))
	if refused.status == 0 {
		logf("the first identifier was never refused within %d requests: nothing to replay, the session counter cannot be measured", *budget)
		s0 = ""
	}

	for i := 1; i < *cells; i++ {
		if *switchCmd == "" {
			logf("no -switch-cmd: cannot visit another address, stopping at %d cell(s)", i)
			break
		}
		if err := runSwitch(*switchCmd); err != nil {
			fatal("switching the exit address: %v", err)
		}
		next, err := p.waitFor(address, *wait)
		if err != nil {
			fatal("waiting for the exit address to change: %v", err)
		}
		address = next

		c := cell{Address: address}
		if s0 != "" {
			status := p.one(s0)
			c.Replay = status
			logf("cell %d %s: replaying the spent identifier -> %s", i+1, address, describe(status))
		}

		fresh := newSession()
		c.Session = fresh
		ok, refused := p.burst(fresh, *budget)
		c.Ok, c.Status, c.RetryAfter = ok, refused.status, refused.retryAfter
		logf("cell %d %s: fresh identifier got %d through, then %s", i+1, address, ok, describe(refused.status))
		cells0 = append(cells0, c)
	}

	report := map[string]any{
		"model":  p.model,
		"cells":  cells0,
		"probed": time.Now().Format(time.RFC3339),
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal("encoding the report: %v", err)
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(encoded, '\n'), 0o600); err != nil {
			fatal("writing %s: %v", *out, err)
		}
	}
	fmt.Println(string(encoded))
	if !*jsonOnly {
		fmt.Fprint(os.Stderr, verdict(cells0))
	}
}

// probe is one run of the measurement.
type probe struct {
	base    string
	model   string
	prompt  string
	addrURL string
	client  *http.Client
}

// burst sends requests under one identifier until a refusal or the budget.
func (p *probe) burst(session string, budget int) (int, refusal) {
	ok := 0
	for i := 0; i < budget; i++ {
		r := p.send(session)
		if r.status == http.StatusOK {
			ok++
			continue
		}
		return ok, r
	}
	return ok, refusal{}
}

// one sends a single request and reports its status.
func (p *probe) one(session string) int { return p.send(session).status }

// refusal is what stopped a burst.
type refusal struct {
	status     int
	retryAfter int
}

// send performs one request.
func (p *probe) send(session string) refusal {
	body, _ := json.Marshal(map[string]any{
		"model":       p.model,
		"stream":      false,
		"max_tokens":  16,
		"temperature": 0,
		"messages":    []map[string]string{{"role": "user", "content": p.prompt}},
	})
	req, err := http.NewRequest(http.MethodPost, p.base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		fatal("building the request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-opencode-session-id", session)
	req.Header.Set("x-opencode-session", session)

	resp, err := p.client.Do(req)
	if err != nil {
		fatal("sending: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	r := refusal{status: resp.StatusCode}
	if v := resp.Header.Get("Retry-After"); v != "" {
		fmt.Sscanf(v, "%d", &r.retryAfter)
	}
	if r.status != http.StatusOK {
		fmt.Fprintf(os.Stderr, "  %d %s\n", r.status, strings.TrimSpace(string(payload)))
	}
	return r
}

// address asks the echo service which address the traffic leaves from.
func (p *probe) address() (string, error) {
	resp, err := p.client.Get(p.addrURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ip="); ok {
			return v, nil
		}
	}
	return "", errors.New("no ip= line in the echo answer")
}

// waitFor polls the echo service until the address differs from the previous
// one.
func (p *probe) waitFor(previous string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		got, err := p.address()
		if err == nil {
			last = got
			if got != previous {
				return got, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("address stayed %q for %s", last, timeout)
}

// runSwitch runs the operator's command that changes the exit.
func runSwitch(command string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// verdict prints the reading of the measurements.
func verdict(cells []cell) string {
	if len(cells) < 2 {
		return "\nno second address was measured: the session counter is not decided by this run\n"
	}
	var b strings.Builder
	b.WriteString("\nreading:\n")
	replayed, accepted, refused := 0, 0, 0
	for _, c := range cells[1:] {
		if c.Replay == 0 {
			continue
		}
		replayed++
		if c.Replay == http.StatusOK {
			accepted++
		} else {
			refused++
		}
	}
	switch {
	case replayed == 0:
		b.WriteString("  the spent identifier was never replayed\n")
	case accepted == replayed:
		b.WriteString("  the session identifier does not carry the refusal across addresses:\n")
		b.WriteString("  a new address recovers the conversation, the identifier may be kept\n")
	case refused == replayed:
		b.WriteString("  the session identifier DOES carry the refusal across addresses:\n")
		b.WriteString("  a spent conversation must leave under a new name once the address changes\n")
	default:
		b.WriteString("  the results disagree between cells: run more cells before trusting either reading\n")
	}
	return b.String()
}

// describe renders a status for the log.
func describe(status int) string {
	if status == 0 {
		return "no refusal"
	}
	return fmt.Sprintf("status %d", status)
}

// newSession mints an identifier in the shape the service accepts.
func newSession() string {
	var b strings.Builder
	b.WriteString("ses_")
	// The first twelve characters are hex: the service reads them as the
	// creation instant, and a value outside the range would look invented.
	now := time.Now().UnixMilli() & 0xFFFFFFFFFFFF
	for i := 0; i < 12; i++ {
		digit := (now >> (4 * (11 - i))) & 0xF
		b.WriteByte("0123456789abcdef"[digit])
	}
	seed := time.Now().UnixNano()
	for i := 0; i < 14; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		b.WriteByte(sessionAlphabet[int((seed>>33)%int64(len(sessionAlphabet)))])
	}
	return b.String()
}

// fatal reports a failure that makes the run meaningless.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "laneprobe: "+format+"\n", args...)
	os.Exit(1)
}
