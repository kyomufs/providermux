package antigravity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Transcode implements provider.Transcoder.
//
// The service answers with Gemini's event stream: `candidates[].content.parts`,
// each part either text, a `thought`-flagged text or a `functionCall`. The client
// side of this gateway speaks OpenAI chunks, and everything downstream — the
// pass-through, the watchdog and the collection of a non-streaming answer — reads
// that shape only. So the translation happens here, once, on the body.
//
// WHAT THE TRANSLATION MUST NOT DO. It does not invent an answer, reorder parts,
// or swallow an error: a stream whose upstream failed ends the reader with an
// error, so the client sees a truncated answer rather than a short one that looks
// complete.
//
// Source of the shape being read: dsh-tool-antigravity 0.5.3, src/llm-adapter.ts
// (findParts) — parts live at `parts`, `content.parts`, `candidates[].content.parts`
// or `serverContent.modelTurn.parts`, and the event is either `{"response": {...}}`
// or the object itself.
func (p *Provider) Transcode(upstream io.ReadCloser, model string) io.ReadCloser {
	scanner := bufio.NewScanner(upstream)
	// A reasoning event of a large answer is not small: the default limit would
	// cut a line in the middle and turn a healthy stream into a parse error.
	scanner.Buffer(make([]byte, 64<<10), 4<<20)

	return &eventStream{
		src:        upstream,
		scanner:    scanner,
		id:         "chatcmpl-" + mintID(),
		model:      model,
		created:    time.Now().Unix(),
		signatures: p.signatures,
	}
}

// eventStream is the translated body.
type eventStream struct {
	src     io.ReadCloser
	scanner *bufio.Scanner

	id      string
	model   string
	created int64

	// out is the translated bytes not yet handed to the reader.
	out []byte
	// started marks that the opening chunk carrying the role was emitted.
	started bool
	// calls counts the tool calls seen, which decides the finish reason of a
	// stream that ends with a call rather than with text.
	calls int
	// finish is the reason named by the upstream, already translated.
	finish string
	// usage is the counter object of the answer, if the upstream sent one.
	usage map[string]any
	// done marks that the closing chunk and the [DONE] marker were emitted.
	done bool
	// saw marks that at least one event arrived: a body with none of them is a
	// shape this route never promised, and an empty answer would hide it.
	saw bool
	// failure is the reason the upstream stopped early.
	failure error
	// signatures is where the thought signature of every emitted call is
	// kept, so that the history can carry it back.
	signatures *signatureStore
}

// Read hands out translated bytes, pulling events from the upstream as needed.
func (s *eventStream) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.failure != nil {
			return 0, s.failure
		}
		if s.done {
			return 0, io.EOF
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.failure = err
				return 0, err
			}
			// The upstream closed without a [DONE] marker: the stream ended the
			// way the service ends it, and the answer is complete.
			s.close()
			continue
		}
		line := s.scanner.Text()
		payload, ok := sseData(line)
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			s.close()
			continue
		}
		s.event(payload)
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

// Close releases the upstream body.
func (s *eventStream) Close() error { return s.src.Close() }

// close emits the closing chunk, the counters and the done marker.
func (s *eventStream) close() {
	if !s.saw {
		s.failure = fmt.Errorf("antigravity: the upstream stream carried no events")
		return
	}
	s.start()
	if s.calls > 0 && (s.finish == "" || s.finish == "stop") {
		s.finish = "tool_calls"
	}
	if s.finish == "" {
		s.finish = "stop"
	}
	s.write(chunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []choice{{Index: 0, Delta: delta{}, FinishReason: &s.finish}},
	})
	if s.usage != nil {
		// The counters travel in a chunk of their own with no choices, which is
		// what a client that asked for them expects: a delta carrying usage would
		// be read as content by a client that did not.
		s.write(map[string]any{
			"id":      s.id,
			"object":  "chat.completion.chunk",
			"created": s.created,
			"model":   s.model,
			"choices": []any{},
			"usage":   s.usage,
		})
	}
	s.out = append(s.out, "data: [DONE]\n\n"...)
	s.done = true
}

// start emits the opening chunk with the role.
func (s *eventStream) start() {
	if s.started {
		return
	}
	s.started = true
	s.write(chunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []choice{{Index: 0, Delta: delta{Role: "assistant"}}},
	})
}

// event translates one upstream event.
func (s *eventStream) event(payload string) {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		// A malformed event is skipped: the parts that did parse are still the
		// model's answer, and a client is better served by it than by nothing.
		return
	}
	if message, ok := envelope["error"].(map[string]any); ok {
		s.failure = fmt.Errorf("antigravity: upstream error: %s", errorText(message))
		return
	}
	if inner, ok := envelope["response"].(map[string]any); ok {
		envelope = inner
	}

	s.saw = true
	s.start()
	s.parts(envelope)
	s.counters(envelope)
	s.finishReason(envelope)
}

// parts emits the content of one event.
func (s *eventStream) parts(envelope map[string]any) {
	for _, part := range collectParts(envelope) {
		if call, ok := part["functionCall"].(map[string]any); ok {
			// The signature sits BESIDE the call, not inside it: it is a field
			// of the part, and the call is one field of the same part.
			signature, _ := part["thoughtSignature"].(string)
			s.functionCall(call, signature)
			continue
		}
		text, ok := part["text"].(string)
		if !ok || text == "" {
			continue
		}
		thought, _ := part["thought"].(bool)
		d := delta{Content: text}
		if thought {
			d.ReasoningContent = text
			d.Content = ""
		}
		s.write(chunk{
			ID:      s.id,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []choice{{Index: 0, Delta: d}},
		})
	}
}

// functionCall emits one tool call.
//
// The arguments are JSON inside the event and a STRING on the OpenAI surface: a
// client parses the string itself, and handing it the structure would give it
// something no parser of that surface reads.
//
// The thought signature has no place on that surface at all, so it is kept here
// and replayed when the client sends the call back in the history.
func (s *eventStream) functionCall(call map[string]any, signature string) {
	name, _ := call["name"].(string)
	id, _ := call["id"].(string)
	if id == "" {
		id = "call_" + mintID()
	}
	s.signatures.put(id, signature)
	args := "{}"
	if raw, ok := call["args"]; ok && raw != nil {
		if encoded, err := json.Marshal(raw); err == nil {
			args = string(encoded)
		}
	}
	s.write(chunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []choice{{
			Index: 0,
			Delta: delta{ToolCalls: []toolCall{{
				Index: s.calls,
				ID:    id,
				Type:  "function",
				Function: functionCall{
					Name:      name,
					Arguments: args,
				},
			}}},
		}},
	})
	s.calls++
}

// counters reads the usage of an event, if it carries any.
func (s *eventStream) counters(envelope map[string]any) {
	meta, ok := envelope["usageMetadata"].(map[string]any)
	if !ok {
		return
	}
	number := func(key string) int {
		if v, ok := meta[key].(float64); ok {
			return int(v)
		}
		return 0
	}
	prompt := number("promptTokenCount")
	reply := number("candidatesTokenCount") + number("thoughtsTokenCount")
	total := number("totalTokenCount")
	if total == 0 {
		total = prompt + reply
	}
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": reply,
		"total_tokens":      total,
	}
	if cached := number("cachedContentTokenCount"); cached > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	s.usage = usage
}

// finishReason reads the reason a candidate stopped.
func (s *eventStream) finishReason(envelope map[string]any) {
	candidates, ok := envelope["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		return
	}
	raw, _ := candidate["finishReason"].(string)
	if reason, ok := openAIFinish(raw); ok {
		s.finish = reason
	}
}

// openAIFinish maps the service's reason onto the client's.
func openAIFinish(raw string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "":
		return "", false
	case "STOP":
		return "stop", true
	case "MAX_TOKENS":
		return "length", true
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter", true
	default:
		return "stop", true
	}
}

// collectParts finds the parts of an event wherever this service puts them.
//
// The same answer arrives in more than one shape depending on the route: the
// wrapped candidate form is the ordinary one, the flat form appears on events that
// carry no candidate, and a tool call can arrive as a `modelTurn`.
func collectParts(envelope map[string]any) []map[string]any {
	if list, ok := envelope["parts"].([]any); ok {
		return asParts(list)
	}
	candidates, ok := envelope["candidates"].([]any)
	if ok {
		for _, raw := range candidates {
			candidate, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if parts := partsOf(candidate); len(parts) > 0 {
				return parts
			}
		}
	}
	if parts := partsOf(envelope); len(parts) > 0 {
		return parts
	}
	return nil
}

// partsOf reads the parts of one candidate-shaped object.
func partsOf(node map[string]any) []map[string]any {
	if content, ok := node["content"].(map[string]any); ok {
		if list, ok := content["parts"].([]any); ok {
			return asParts(list)
		}
	}
	if modelTurn, ok := node["modelTurn"].(map[string]any); ok {
		return partsOf(modelTurn)
	}
	return nil
}

// asParts narrows a decoded list to objects.
func asParts(list []any) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		if part, ok := raw.(map[string]any); ok {
			out = append(out, part)
		}
	}
	return out
}

// errorText extracts a message from an error envelope.
func errorText(node map[string]any) string {
	if text, ok := node["message"].(string); ok && text != "" {
		return text
	}
	if message, ok := node["status"].(string); ok && message != "" {
		return message
	}
	return "unknown error"
}

// sseData extracts the payload of an event line.
func sseData(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

// write appends one translated event to the output buffer.
func (s *eventStream) write(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(raw)
	buf.WriteString("\n\n")
	s.out = append(s.out, buf.Bytes()...)
}

// chunk is one OpenAI event.
type chunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
}

// choice is one completion alternative. There is only ever one.
type choice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// delta is the piece of the answer this event carries.
type delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
}

// toolCall is one tool call, whole: this service sends no fragments of it.
type toolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

// functionCall is the callable part of a tool call.
type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
