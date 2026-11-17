package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"providermux/internal/provider"
)

// clientDeclaration returns the client's own declaration for name, in the
// neutral form the provider takes from a caller.
func clientDeclaration(t *testing.T, name string) provider.Tool {
	t.Helper()
	fn, ok := clientTools[name]
	if !ok {
		t.Fatalf("tools.json has no %q", name)
	}
	description, _ := fn["description"].(string)
	schema, err := json.Marshal(fn["parameters"])
	if err != nil {
		t.Fatalf("marshal %q parameters: %v", name, err)
	}
	return provider.Tool{Name: name, Description: description, Schema: schema}
}

// TestClientDeclarationIsKept — the client's own tool must go out under its own
// name. Reserving the name only means that a DIFFERENT declaration of it is
// moved aside; rewriting the declaration the lane admits would move the request
// away from the client it imitates.
func TestClientDeclarationIsKept(t *testing.T) {
	p := newTestProvider(t)
	prep, err := p.Build(provider.Request{
		Model: provider.Model{ID: "m"},
		Tools: []provider.Tool{clientDeclaration(t, "write")},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if names := toolNamesFromBody(t, prep.Body); !contains(names, "write") {
		t.Fatalf("the client's own declaration was renamed: %v", names)
	}
	if len(prep.ToolNames) != 0 {
		t.Fatalf("a translation was reported for an untouched declaration: %v", prep.ToolNames)
	}
}

// TestForeignDeclarationOfClientNameIsAliased — MEASURED 2026-11-17: the lane
// answers 429 when a client tool name carries a foreign declaration, and 200
// for the same declaration under a name the client does not ship. The caller's
// tool keeps working because the provider reports the alias for the answer.
func TestForeignDeclarationOfClientNameIsAliased(t *testing.T) {
	p := newTestProvider(t)
	foreign := provider.Tool{
		Name:        "edit",
		Description: "Edit an existing UTF-8 text file",
		Schema:      []byte(`{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}}}`),
	}
	prep, err := p.Build(provider.Request{
		Model: provider.Model{ID: "m"},
		Tools: []provider.Tool{foreign},
		Messages: []provider.Message{
			{Role: "user", Text: "go"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "edit", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "call_1", Text: "ok"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	names := toolNamesFromBody(t, prep.Body)
	if contains(names, "edit") {
		t.Fatalf("a foreign declaration went out under the client's name: %v", names)
	}
	if !contains(names, "edit_tool") {
		t.Fatalf("the call the client asked for is not on the wire: %v", names)
	}
	if got := prep.ToolNames["edit_tool"]; got != "edit" {
		t.Fatalf("ToolNames[edit_tool] = %q, want edit", got)
	}
	// The history has to name the tool the request declares: a declaration and
	// a call it does not match is a different conversation.
	if !strings.Contains(string(prep.Body), `"name":"edit_tool"`) {
		t.Fatal("the historical tool call still names the caller's tool")
	}
}

// TestAliasCollisionIsResolved — the alias suffix cannot shadow a tool the
// caller already has under that name.
func TestAliasCollisionIsResolved(t *testing.T) {
	p := newTestProvider(t)
	prep, err := p.Build(provider.Request{
		Model: provider.Model{ID: "m"},
		Tools: []provider.Tool{
			{Name: "edit", Description: "someone else's edit", Schema: []byte(`{"type":"object"}`)},
			{Name: "edit_tool", Description: "the caller's own", Schema: []byte(`{"type":"object"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	names := toolNamesFromBody(t, prep.Body)
	for _, want := range []string{"edit_tool", "edit_tool_2"} {
		if !contains(names, want) {
			t.Fatalf("tool %q is missing from the wire: %v", want, names)
		}
	}
	if got := prep.ToolNames["edit_tool_2"]; got != "edit" {
		t.Fatalf("ToolNames[edit_tool_2] = %q, want edit", got)
	}
	if got, replaced := prep.ToolNames["edit_tool"]; replaced {
		t.Fatalf("the caller's own edit_tool was translated to %q", got)
	}
}

// TestToolDescriptionSignaturesAreScrubbed — MEASURED 2026-11-17: these phrases
// are refused whatever the tool is called, so they are removed from every
// description, not only from a client name's.
func TestToolDescriptionSignaturesAreScrubbed(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Load a skill from the session skill catalog.", "Load a skill from the skill catalog."},
		{"Edit an existing UTF-8 text file", "Edit a text file"},
		{"Search inside the session skill catalog", "Search inside the skill catalog"},
		{"nothing to do", "nothing to do"},
	}
	for _, tc := range cases {
		if got := scrubToolDescription(tc.in); got != tc.want {
			t.Errorf("scrubToolDescription(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDescriptionScrubAppliesOnTheWire — the scrub belongs to the request
// assembly, not to the helper: an unknown tool name is not refused for its
// phrase, but a client name is, and both carry the same entry builder.
func TestDescriptionScrubAppliesOnTheWire(t *testing.T) {
	p := newTestProvider(t)
	prep, err := p.Build(provider.Request{
		Model: provider.Model{ID: "m"},
		Tools: []provider.Tool{{
			Name:        "find_text",
			Description: "Find text. Read more in the session skill catalog.",
			Schema:      []byte(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(string(prep.Body), "in the skill catalog") || strings.Contains(string(prep.Body), "session skill catalog") {
		t.Fatalf("the refused phrase went on the wire: %s", prep.Body)
	}
}
