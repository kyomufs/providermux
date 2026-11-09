package antigravity

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"providermux/internal/provider"
)

// newTestProvider builds a provider that does not need a network.
func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// TestBuildUsesTheRouteOfTheEffort — the effort is not a field of the request; it
// is part of the model name on the wire. A body with the base name is refused by
// the service, so the route and the thinking setting must agree.
func TestBuildUsesTheRouteOfTheEffort(t *testing.T) {
	p := newTestProvider(t)

	cases := []struct {
		effort string
		wire   string
		enum   string
		budget string
	}{
		{"low", "gemini-3.8-flash-low", "MODEL_PLACEHOLDER_M320", "1000"},
		{"medium", "gemini-3.8-flash-medium", "MODEL_PLACEHOLDER_M319", "4000"},
		{"high", "gemini-3.8-flash-high", "MODEL_PLACEHOLDER_M318", "<nil>"},
		{"", "gemini-3.8-flash-medium", "MODEL_PLACEHOLDER_M319", "4000"},
		{"bogus", "gemini-3.8-flash-medium", "MODEL_PLACEHOLDER_M319", "4000"},
	}

	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			prep, err := p.Build(provider.Request{
				Model:           provider.Model{ID: "gemini-3.8-flash"},
				ReasoningEffort: tc.effort,
				Messages:        []provider.Message{{Role: "user", Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(prep.Body, &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if got := body["model"]; got != tc.wire {
				t.Fatalf("model = %v, want %q", got, tc.wire)
			}

			request, _ := body["request"].(map[string]any)
			labels, _ := request["labels"].(map[string]any)
			if got := labels["model_enum"]; got != tc.enum {
				t.Fatalf("model_enum = %v, want %q", got, tc.enum)
			}
			gen, _ := request["generationConfig"].(map[string]any)
			thinking, _ := gen["thinkingConfig"].(map[string]any)
			if got := fmt.Sprint(thinking["thinkingBudget"]); got != tc.budget {
				t.Fatalf("thinkingBudget = %v, want %v", got, tc.budget)
			}
			if thinking["includeThoughts"] != true {
				t.Fatalf("includeThoughts = %v, want true", thinking["includeThoughts"])
			}
		})
	}
}

// TestBuildAsksForTheStream — the gateway collects the events into a single answer
// for a client that asked for one, so the stream form is the only form requested.
func TestBuildAsksForTheStream(t *testing.T) {
	p := newTestProvider(t)

	prep, err := p.Build(provider.Request{Model: provider.Model{ID: "gpt-oss-120b-medium"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(prep.URL, ":streamGenerateContent") {
		t.Fatalf("url = %q, want the streaming route", prep.URL)
	}
	if !prep.StreamsUpstream {
		t.Fatal("StreamsUpstream = false: the gateway would wait for a whole body")
	}
	if got := prep.Headers.Get("user-agent"); got != DefaultUserAgent {
		t.Fatalf("user-agent = %q, want the client identity", got)
	}
	if got := prep.Headers.Get("authorization"); got != "" {
		t.Fatalf("authorization = %q: the credential is set by the shared machinery", got)
	}
}

// TestBuildFoldsTheToolResultIntoTheUserTurn — the service has no tool role, and a
// result that arrives under one is a user turn with a functionResponse part.
func TestBuildFoldsTheToolResultIntoTheUserTurn(t *testing.T) {
	p := newTestProvider(t)

	prep, err := p.Build(provider.Request{
		Model:  provider.Model{ID: "gemini-4-argon"},
		System: "be terse",
		Messages: []provider.Message{
			{Role: "user", Text: "search"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "grep", Arguments: `{"pattern":"x"}`}}},
			{Role: "tool", ToolCallID: "c1", Text: "found"},
		},
		Tools: []provider.Tool{{
			Name:        "grep",
			Description: "search",
			Schema:      []byte(`{"type":"object","properties":{"pattern":{"type":"string"}},"additionalProperties":false}`),
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(prep.Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	request, _ := body["request"].(map[string]any)

	contents, _ := request["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d turns, want 3: %s", len(contents), prep.Body)
	}
	last, _ := contents[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("tool result role = %v, want user", last["role"])
	}
	parts, _ := last["parts"].([]any)
	part, _ := parts[0].(map[string]any)
	response, _ := part["functionResponse"].(map[string]any)
	if response["name"] != "grep" {
		t.Fatalf("functionResponse name = %v, want grep", response["name"])
	}

	system, _ := request["systemInstruction"].(map[string]any)
	systemParts, _ := system["parts"].([]any)
	systemPart, _ := systemParts[0].(map[string]any)
	if systemPart["text"] != "be terse" {
		t.Fatalf("systemInstruction = %v", request["systemInstruction"])
	}

	tools, _ := request["tools"].([]any)
	wrapper, _ := tools[0].(map[string]any)
	decls, _ := wrapper["functionDeclarations"].([]any)
	decl, _ := decls[0].(map[string]any)
	parameters, _ := decl["parameters"].(map[string]any)
	if parameters["type"] != "OBJECT" {
		t.Fatalf("schema type = %v, want the upper-case spelling", parameters["type"])
	}
	if _, ok := parameters["additionalProperties"]; ok {
		t.Fatal("additionalProperties reached the schema: the service rejects it")
	}
}

// TestModelsAdvertiseTheLadderTheyHave — an effort the model has no route for must
// not be advertised: it would be answered with the default and read as ignored.
func TestModelsAdvertiseTheLadderTheyHave(t *testing.T) {
	p := newTestProvider(t)

	models, err := p.Models(t.Context())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("the catalog is empty")
	}
	for _, m := range models {
		if m.ID == "gpt-oss-120b-medium" || m.ID == "claude-sonnet-4-6" {
			if len(m.ReasoningOptions) != 1 || len(m.ReasoningOptions[0].Values) != 1 {
				t.Fatalf("ladder of %q = %+v, want a single medium route", m.ID, m.ReasoningOptions)
			}
			continue
		}
		if len(m.ReasoningOptions) != 1 || len(m.ReasoningOptions[0].Values) != 3 {
			t.Fatalf("ladder of %q = %+v, want low/medium/high", m.ID, m.ReasoningOptions)
		}
	}
}

// TestSessionIDIsTheSignedHashOfTheWorkspace — the identifier decides the
// conversation the service files the turns under, and its sign is part of the
// value: the unsigned reading names a different conversation.
func TestSessionIDIsTheSignedHashOfTheWorkspace(t *testing.T) {
	p := newTestProvider(t)

	prep, err := p.Build(provider.Request{
		Model:   provider.Model{ID: "gemini-3.8-flash"},
		Session: provider.SessionIDs{Project: "file:///home/user/project"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(prep.Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	request, _ := body["request"].(map[string]any)
	session, _ := request["sessionId"].(string)
	if session == "" {
		t.Fatal("sessionId is absent")
	}
	for _, r := range session {
		if r != '-' && (r < '0' || r > '9') {
			t.Fatalf("sessionId = %q, want a decimal integer", session)
		}
	}
	if session != signedSessionID("file:///home/user/project") {
		t.Fatalf("sessionId = %q, want the hash of the workspace", session)
	}
}
