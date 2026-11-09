package antigravity

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"providermux/internal/provider"
)

// TestTheSignatureOfAToolCallSurvivesTheRoundTrip — the service signs every
// function call it emits and refuses a replayed call without the signature
// (measured 2026-11-09: 400 INVALID_ARGUMENT, "Function call is missing a
// thought_signature in functionCall parts"). The OpenAI surface has no field
// for it, so the provider keeps it and puts it back when the client returns the
// call in the history.
func TestTheSignatureOfAToolCallSurvivesTheRoundTrip(t *testing.T) {
	const signature = "EowCCokCAWkUfRM+viUcgyKWwAwQC3pes4MN"
	p := newTestProvider(t)

	upstream := `data: {"response":{"candidates":[{"content":{"parts":[{"thoughtSignature":"` + signature + `","functionCall":{"name":"skill","args":{"name":"brainstorming"},"id":"call_42"}}]},"finishReason":"STOP"}]}}

`
	body := p.Transcode(io.NopCloser(strings.NewReader(upstream)), "gemini-3.8-flash")
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("reading the translated stream: %v", err)
	}

	prepared, err := p.Build(provider.Request{
		Model: modelOf(t, p, "gemini-3.8-flash"),
		Messages: []provider.Message{
			{Role: "user", Text: "run the skill"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "call_42", Name: "skill", Arguments: `{"name":"brainstorming"}`}}},
			{Role: "tool", ToolCallID: "call_42", Text: "done"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	parts := functionCallParts(t, prepared.Body)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	if got, _ := parts[0]["thoughtSignature"].(string); got != signature {
		t.Fatalf("thoughtSignature = %q, want the one the service issued", got)
	}
}

// TestAnUnknownCallGetsTheSentinel — a call this process never saw answered
// (a restart, a trimmed history) still has to carry a signature, and the vendor
// documents the sentinel for exactly that case.
func TestAnUnknownCallGetsTheSentinel(t *testing.T) {
	p := newTestProvider(t)

	prepared, err := p.Build(provider.Request{
		Model: modelOf(t, p, "gemini-3.8-flash"),
		Messages: []provider.Message{
			{Role: "user", Text: "run the skill"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "call_from_before", Name: "skill", Arguments: `{}`}}},
			{Role: "tool", ToolCallID: "call_from_before", Text: "done"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	parts := functionCallParts(t, prepared.Body)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	if got, _ := parts[0]["thoughtSignature"].(string); got != skipThoughtSignature {
		t.Fatalf("thoughtSignature = %q, want %q", got, skipThoughtSignature)
	}
}

// TestAClaudeRunSignsOnlyTheFirstCall — the signature is Claude's own field
// there and a run of sentinels is refused, which is why only the first replayed
// call carries one.
func TestAClaudeRunSignsOnlyTheFirstCall(t *testing.T) {
	p := newTestProvider(t)

	prepared, err := p.Build(provider.Request{
		Model: modelOf(t, p, "claude-sonnet-4-6"),
		Messages: []provider.Message{
			{Role: "user", Text: "two calls"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{
				{ID: "call_a", Name: "one", Arguments: `{}`},
				{ID: "call_b", Name: "two", Arguments: `{}`},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	parts := functionCallParts(t, prepared.Body)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if _, ok := parts[0]["thoughtSignature"]; !ok {
		t.Fatal("the first call carries no signature")
	}
	if sig, ok := parts[1]["thoughtSignature"]; ok {
		t.Fatalf("the second call carries %v, want none", sig)
	}
}

// modelOf returns the model with the given identifier.
func modelOf(t *testing.T, p *Provider, id string) provider.Model {
	t.Helper()
	models, err := p.Models(t.Context())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	for _, m := range models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("model %q is not declared", id)
	return provider.Model{}
}

// functionCallParts pulls the replayed functionCall parts out of a body.
func functionCallParts(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var payload struct {
		Request struct {
			Contents []struct {
				Parts []map[string]any `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	out := make([]map[string]any, 0)
	for _, content := range payload.Request.Contents {
		for _, part := range content.Parts {
			if _, ok := part["functionCall"]; ok {
				out = append(out, part)
			}
		}
	}
	return out
}
