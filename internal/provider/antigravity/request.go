package antigravity

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"providermux/internal/provider"
)

// buildContents translates the neutral messages into the service's `contents`.
//
// The shape is Gemini's, with one difference that matters: a tool result is not a
// message of its own role but a `functionResponse` part inside a user turn. The
// service has three roles — user, model — and folds the tool role into user
// parts, which is why consecutive user turns are merged instead of appended.
func buildContents(req provider.Request, claude bool, signatures *signatureStore) []any {
	names := callNames(req.Messages)

	out := make([]any, 0, len(req.Messages))
	appendContent := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if n := len(out); n > 0 {
			last, _ := out[n-1].(map[string]any)
			if last != nil && last["role"] == role {
				existing, _ := last["parts"].([]any)
				last["parts"] = append(existing, parts...)
				return
			}
		}
		out = append(out, map[string]any{"role": role, "parts": parts})
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "assistant":
			parts := make([]any, 0, len(msg.ToolCalls)+1)
			// The thinking of an earlier turn is not sent back. The service
			// expects a signature beside a thought part and refuses a replayed
			// one without it, and the signature is not part of the neutral
			// message the gateway hands over. Thinking is not part of the
			// conversation as the model sees it on the next turn.
			if text := msg.Text; text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
			// A replayed call has to carry the signature the service issued for
			// it, and the OpenAI surface has no field to carry it in. Without
			// one the request is refused: "Function call is missing a
			// thought_signature in functionCall parts" (measured 2026-11-09).
			// The signature of a call the service has already answered is
			// replayed as it was; the sentinel covers the calls whose signature
			// this process never saw.
			//
			// Claude takes the sentinel only on the first replayed call: the
			// signature is its own field there and a run of them is refused.
			sawClaudeCall := false
			for _, call := range msg.ToolCalls {
				fc := map[string]any{
					"name": call.Name,
					"args": toolArguments(call.Arguments),
				}
				if claude && call.ID != "" {
					fc["id"] = call.ID
				}
				part := map[string]any{"functionCall": fc}
				if !claude || !sawClaudeCall {
					part["thoughtSignature"] = signatures.signatureFor(call.ID)
				}
				sawClaudeCall = sawClaudeCall || claude
				parts = append(parts, part)
			}
			appendContent("model", parts)

		case "tool":
			name := names[msg.ToolCallID]
			if name == "" {
				// A result whose call is not in the conversation would name a
				// function the model never called, and the service refuses the
				// request. Skipping it loses nothing the model can use.
				continue
			}
			fr := map[string]any{
				"name":     name,
				"response": map[string]any{"content": msg.Text},
			}
			if claude && msg.ToolCallID != "" {
				fr["id"] = msg.ToolCallID
			}
			appendContent("user", []any{map[string]any{"functionResponse": fr}})

		default:
			parts := make([]any, 0, len(msg.Images)+1)
			if msg.Text != "" {
				parts = append(parts, map[string]any{"text": msg.Text})
			}
			for _, img := range msg.Images {
				parts = append(parts, map[string]any{
					"inlineData": map[string]any{
						"mimeType": img.MediaType,
						"data":     base64.StdEncoding.EncodeToString(img.Data),
					},
				})
			}
			appendContent("user", parts)
		}
	}
	return out
}

// callNames indexes the tool calls of the conversation by their identifier, so
// that a tool result can be answered under the name the model called.
func callNames(messages []provider.Message) map[string]string {
	out := make(map[string]string)
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.ID != "" {
				out[call.ID] = call.Name
			}
		}
	}
	return out
}

// toolArguments turns the serialized arguments into an object.
//
// The service takes the arguments as structure, not as a string: a function call
// whose arguments are a JSON string is a call with one unnamed field. A body that
// is not an object is replaced rather than refused — a client that sent an empty
// or broken string meant a call with no arguments.
func toolArguments(raw string) any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
		return map[string]any{}
	}
	return args
}

// buildTools translates the declared tools into the service's form.
//
// The wrapper is required: the service accepts `functionDeclarations` and refuses
// the same entries written at the top level of the tool object.
//
// Source: dsh-tool-antigravity 0.5.3, src/llm-adapter.ts (buildFunctionDeclarations)
// and @cortexkit/antigravity-auth-core 3.0.14, dist/transform/gemini.js
// (wrapToolsAsFunctionDeclarations, toGeminiSchema).
func buildTools(tools []provider.Tool) []any {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]any, 0, len(tools))
	for _, tool := range tools {
		decl := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
		}
		if schema := geminiSchema(tool.Schema); schema != nil {
			decl["parameters"] = schema
		}
		decls = append(decls, decl)
	}
	return []any{map[string]any{"functionDeclarations": decls}}
}

// geminiSchema rewrites a JSON schema into the form the service validates.
//
// The service's protobuf validation rejects the OpenAPI leftovers a client may
// send (`$schema`, `additionalProperties`) and takes the type names in upper case,
// so a schema passed through as-is is refused with an error naming a field nobody
// wrote.
func geminiSchema(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	return normalizeSchema(schema)
}

// allowedGeminiSchemaFields defines the fields accepted by Gemini's protobuf Schema.
var allowedGeminiSchemaFields = map[string]bool{
	"type":             true,
	"properties":       true,
	"items":            true,
	"required":         true,
	"description":      true,
	"format":           true,
	"pattern":          true,
	"title":            true,
	"nullable":         true,
	"propertyOrdering": true,
	"example":          true,
	"default":          true,
	"enum":             true,
	"anyOf":            true,
	"minimum":          true,
	"maximum":          true,
	"minLength":        true,
	"maxLength":        true,
	"minItems":         true,
	"maxItems":         true,
	"minProperties":    true,
	"maxProperties":    true,
}

// normalizeSchema walks a schema and applies the rewrites required by the
// service: upper-case types, const -> enum, union flattening, and filtering
// of unknown fields rejected by Gemini's protobuf Schema validator.
func normalizeSchema(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, value := range node {
			switch key {
			case "$schema", "additionalProperties", "$id", "$comment", "definitions", "$defs":
				// Not part of the schema dialect the service validates.
				continue
			case "const":
				// Gemini protobuf Schema has no "const" field; convert string const to enum.
				if s, ok := value.(string); ok {
					if _, hasEnum := node["enum"]; !hasEnum {
						out["enum"] = []any{s}
					}
				}
				continue
			case "oneOf", "anyOf":
				// Handled below by union flattening.
				continue
			case "properties":
				if props, ok := value.(map[string]any); ok {
					normalizedProps := make(map[string]any, len(props))
					for propName, propSchema := range props {
						normalizedProps[propName] = normalizeSchema(propSchema)
					}
					out[key] = normalizedProps
					continue
				}
				out[key] = normalizeSchema(value)
			case "type":
				if s, ok := value.(string); ok {
					out[key] = strings.ToUpper(s)
					continue
				}
				// A union of types is expressed in this dialect as a list; the
				// service takes the first named type, and the nullable marker
				// carries the rest.
				if list, ok := value.([]any); ok {
					for _, item := range list {
						if s, ok := item.(string); ok && s != "null" {
							out[key] = strings.ToUpper(s)
							break
						}
					}
					continue
				}
				out[key] = value
			default:
				if allowedGeminiSchemaFields[key] {
					out[key] = normalizeSchema(value)
				}
			}
		}

		// If the node defines a union (oneOf / anyOf), flatten it into a single non-polymorphic
		// schema so that both Gemini and Claude on Vertex AI accept it without schema validation errors.
		var unionBranches []any
		if rawOneOf, ok := node["oneOf"].([]any); ok && len(rawOneOf) > 0 {
			unionBranches = rawOneOf
		} else if rawAnyOf, ok := node["anyOf"].([]any); ok && len(rawAnyOf) > 0 {
			unionBranches = rawAnyOf
		}
		if len(unionBranches) > 0 {
			return flattenUnion(unionBranches, out)
		}

		return out
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = normalizeSchema(item)
		}
		return out
	default:
		return v
	}
}

// flattenUnion collapses anyOf/oneOf branches into a single schema.
// Anthropic Claude on Vertex AI converts functionDeclarations into its own schema
// and strictly rejects anyOf/oneOf unions (especially inside array items, e.g. generate_image).
func flattenUnion(branches []any, base map[string]any) map[string]any {
	var nonNull []map[string]any
	for _, b := range branches {
		bm, ok := normalizeSchema(b).(map[string]any)
		if !ok {
			continue
		}
		if t, ok := bm["type"].(string); ok && strings.EqualFold(t, "NULL") {
			base["nullable"] = true
			continue
		}
		nonNull = append(nonNull, bm)
	}

	if len(nonNull) == 0 {
		return base
	}

	// Case 1: All branches are string enums / constants -> combine into a single STRING enum.
	allEnums := true
	var enumValues []any
	for _, b := range nonNull {
		enums, hasEnum := b["enum"].([]any)
		if !hasEnum || b["properties"] != nil || b["items"] != nil {
			allEnums = false
			break
		}
		enumValues = append(enumValues, enums...)
	}
	if allEnums && len(enumValues) > 0 {
		base["type"] = "STRING"
		base["enum"] = dedupeAny(enumValues)
		return base
	}

	// Case 2: Object branches -> merge their properties into a single OBJECT schema.
	allObjects := true
	for _, b := range nonNull {
		t, _ := b["type"].(string)
		if t != "OBJECT" && b["properties"] == nil {
			allObjects = false
			break
		}
	}
	if allObjects {
		base["type"] = "OBJECT"
		mergedProps := make(map[string]any)
		for _, b := range nonNull {
			if props, ok := b["properties"].(map[string]any); ok {
				for pk, pv := range props {
					if existing, exists := mergedProps[pk]; exists {
						if em, ok := existing.(map[string]any); ok {
							if pm, ok := pv.(map[string]any); ok {
								if ee, ok1 := em["enum"].([]any); ok1 {
									if pe, ok2 := pm["enum"].([]any); ok2 {
										em["enum"] = dedupeAny(append(ee, pe...))
									}
								}
							}
						}
					} else {
						mergedProps[pk] = pv
					}
				}
			}
		}
		base["properties"] = mergedProps
		return base
	}

	// Case 3: Mixed types -> choose highest priority branch (object > array > primitive).
	score := func(b map[string]any) int {
		if b["properties"] != nil || b["type"] == "OBJECT" {
			return 3
		}
		if b["items"] != nil || b["type"] == "ARRAY" {
			return 2
		}
		return 1
	}
	best := nonNull[0]
	for _, b := range nonNull[1:] {
		if score(b) > score(best) {
			best = b
		}
	}
	for k, v := range best {
		if _, exists := base[k]; !exists {
			base[k] = v
		}
	}
	return base
}

func dedupeAny(in []any) []any {
	seen := make(map[string]bool, len(in))
	var out []any
	for _, item := range in {
		s := fmt.Sprint(item)
		if !seen[s] {
			seen[s] = true
			out = append(out, item)
		}
	}
	return out
}

// generationConfig assembles the model's answer settings.
func generationConfig(req provider.Request, def modelDef, route tier) map[string]any {
	out := map[string]any{"maxOutputTokens": def.maxOutput}
	if req.MaxTokens > 0 {
		out["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}

	thinking := map[string]any{"includeThoughts": true}
	if route.level != "" {
		thinking["thinkingLevel"] = route.level
	}
	if route.budget > 0 {
		thinking["thinkingBudget"] = route.budget
	}
	if def.claude {
		// The adaptive Claude routes keep an explicit zero budget beside the
		// level: a level alone is read as legacy thinking and ignored.
		thinking["thinkingBudget"] = 0
	}
	out["thinkingConfig"] = thinking
	return out
}

// signedSessionID hashes the workspace into the identifier the service files the
// conversation under.
//
// It is FNV-1a 64 over the workspace URI, signed, in decimal: the service takes a
// string of digits and the reference client sends the signed reading of the same
// hash, so an unsigned one names a different conversation.
func signedSessionID(workspace string) string {
	if workspace == "" {
		workspace = "file:///workspace"
	}
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(workspace); i++ {
		h ^= uint64(workspace[i])
		h *= prime64
	}
	return strconv.FormatInt(int64(h), 10)
}
