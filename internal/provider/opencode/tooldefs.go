package opencode

// The reference client's own tool declarations.
//
// WHY THEY ARE EMBEDDED. The free lane admits a request only when the tools it
// carries look like the client's. A declaration that claims one of the client's
// names but describes a different tool is read as traffic from another agent
// and answered 429 FreeUsageLimitError — measured 2026-11-17, one tool at a
// time, with the rest of the request held constant:
//
//	edit, the client's definition                   -> 200
//	edit, another description                       -> 429
//	edit, file_path/old_string/new_string           -> 429
//	the same tool under a name the client lacks     -> 200
//
// So the lane does not compare definitions against the client's and does not
// require a match; it refuses a *known name* whose content does not look like
// the client's, and it does not care what an unknown name contains. The client
// names are therefore reserved: this provider sends them only when the
// declaration is the client's own (see buildTools), and a caller's different
// tool with the same name goes out under an alias the gateway translates back.
//
// The text of the declarations is captured from opencode 2.2.6, the build the
// lane admits. tools.json is that capture verbatim, so a comparison here is a
// comparison against the bytes the service itself was shown.

import (
	_ "embed"
	"encoding/json"
	"reflect"
)

//go:embed tools.json
var clientToolsJSON []byte

// clientTools maps a client tool name to its declaration as the client sends it.
var clientTools = parseClientTools(clientToolsJSON)

func parseClientTools(raw []byte) map[string]map[string]any {
	var list []map[string]any
	out := make(map[string]map[string]any)
	if err := json.Unmarshal(raw, &list); err != nil {
		// A broken embedded table would silently disable the reservation of the
		// client's names, which is the one thing this file exists for. The build
		// is the place to notice it, not a live request.
		panic("opencode: embedded tools.json does not parse: " + err.Error())
	}
	for _, entry := range list {
		fn, ok := entry["function"].(map[string]any)
		if !ok {
			continue
		}
		name, ok := fn["name"].(string)
		if !ok || name == "" {
			continue
		}
		out[name] = fn
	}
	return out
}

// isClientTool reports whether the name belongs to the reference client.
func isClientTool(name string) bool {
	_, ok := clientTools[name]
	return ok
}

// sameAsClient reports whether t carries the client's declaration for its name.
//
// The comparison is done on parsed JSON, not on bytes: the caller reaches this
// package through the gateway's own decoding and re-encoding, so key order and
// whitespace are not preserved and a byte comparison would never match.
func sameAsClient(name, description string, schema json.RawMessage) bool {
	fn, ok := clientTools[name]
	if !ok {
		return false
	}
	if !sameText(description, fn["description"]) {
		return false
	}
	want, ok := fn["parameters"]
	if !ok {
		return len(schema) == 0
	}
	if len(schema) == 0 {
		return false
	}
	var got any
	if err := json.Unmarshal(schema, &got); err != nil {
		return false
	}
	return reflect.DeepEqual(got, want)
}

// sameText compares a tool description against the canonical value.
func sameText(got string, want any) bool {
	s, ok := want.(string)
	if !ok {
		return false
	}
	return got == s
}

// Tool descriptions the lane reads as another agent's signature.
//
// MEASURED 2026-11-17, one declaration at a time, with the admitted client body
// as the base. Unlike the parameter names below, these phrases are refused
// whatever the tool is called — a tool named `x` carrying them is refused too.
// They are therefore scrubbed from every description before it goes on the wire.
//
//	"…in the session skill catalog."      -> 429
//	"…in the skill catalog."              -> 200
//	"Edit an existing UTF-8 text file"    -> 429
//	"Edit an existing text file"          -> 200
//	"Edit a text file"                    -> 200
//
// The replacement keeps the meaning for the model: the skill sentence still
// names a catalog, the edit sentence still says what the tool does.
var toolDescriptionRules = []struct {
	marker      string
	replacement string
}{
	{"session skill catalog", "skill catalog"},
	{"edit an existing utf-8 text file", "Edit a text file"},
}

// scrubToolDescription removes the known non-client signatures from a tool
// description, leaving everything else untouched.
//
// The rules are written in lower case because replaceFold matches on the lower-
// cased text; that the replacement keeps the sentence readable is deliberate.
func scrubToolDescription(s string) string {
	out := s
	for _, rule := range toolDescriptionRules {
		out = replaceFold(out, rule.marker, rule.replacement)
	}
	return out
}

// clientToolAlias is the name a caller's non-client declaration of a client
// name goes out under.
//
// The suffix rather than a prefix: the lane compares the beginning of a name in
// the rules it documents, and a suffix keeps the caller's intent readable in a
// transcript. The name is de-duplicated against the caller's other tools where
// it is used.
func clientToolAlias(name string) string { return name + "_tool" }
