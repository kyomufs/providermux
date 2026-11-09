package antigravity

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// translateStream runs a body through the transcoder and returns the events.
func translateStream(t *testing.T, upstream string) []string {
	t.Helper()
	p := newTestProvider(t)
	body := p.Transcode(io.NopCloser(strings.NewReader(upstream)), "gemini-3.8-flash")
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading the translated stream: %v", err)
	}
	events := make([]string, 0)
	for _, line := range strings.Split(string(raw), "\n") {
		if payload, ok := sseData(line); ok {
			events = append(events, payload)
		}
	}
	return events
}

// decoded returns the events as objects, with the done marker left out.
func decoded(t *testing.T, events []string) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(events))
	for _, payload := range events {
		if payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("event is not JSON: %v: %s", err, payload)
		}
		out = append(out, event)
	}
	return out
}

// TestTheAnswerBecomesOpenAIChunks — text, thinking, a tool call, the counters and
// the done marker all reach the client in the shape it parses.
func TestTheAnswerBecomesOpenAIChunks(t *testing.T) {
	upstream := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}]}}]}}

data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}

data: {"response":{"candidates":[{"content":{"parts":[{"text":", world"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}

data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"grep","args":{"pattern":"x"}}}]},"finishReason":"STOP"}]}}

data: [DONE]

`
	events := translateStream(t, upstream)
	if len(events) == 0 || events[len(events)-1] != "[DONE]" {
		t.Fatalf("the stream does not end with the done marker: %v", events)
	}

	var content, reasoning strings.Builder
	var calls, finish, usage string
	for _, event := range decoded(t, events) {
		choices, _ := event["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				content.WriteString(text)
			}
			if text, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(text)
			}
			if list, ok := delta["tool_calls"].([]any); ok {
				raw, _ := json.Marshal(list)
				calls = string(raw)
			}
			if reason, ok := choice["finish_reason"].(string); ok {
				finish = reason
			}
		}
		if counters, ok := event["usage"].(map[string]any); ok {
			raw, _ := json.Marshal(counters)
			usage = string(raw)
		}
	}

	if content.String() != "Hello, world" {
		t.Fatalf("content = %q", content.String())
	}
	if reasoning.String() != "thinking" {
		t.Fatalf("reasoning = %q, want the thought part", reasoning.String())
	}
	if !strings.Contains(calls, `"name":"grep"`) || !strings.Contains(calls, `"arguments":"{\"pattern\":\"x\"}"`) {
		t.Fatalf("tool call = %s, want the arguments as a string", calls)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finish)
	}
	if !strings.Contains(usage, `"prompt_tokens":10`) || !strings.Contains(usage, `"completion_tokens":5`) {
		t.Fatalf("usage = %s", usage)
	}
}

// TestTheRoleGoesOutFirst — an OpenAI client reads the first chunk as the role
// announcement and takes the rest as the answer.
func TestTheRoleGoesOutFirst(t *testing.T) {
	events := decoded(t, translateStream(t, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}}`+"\n\n"))
	if len(events) == 0 {
		t.Fatal("no events")
	}
	choices, _ := events[0]["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Fatalf("first delta = %v, want the role", delta)
	}
}

// TestAMaxTokensStopBecomesLength — the reason a client reacts to is its own.
func TestAMaxTokensStopBecomesLength(t *testing.T) {
	events := decoded(t, translateStream(t, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"MAX_TOKENS"}]}}`+"\n\n"))
	last := events[len(events)-1]
	choices, _ := last["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %v, want length", choice["finish_reason"])
	}
}

// TestAnUpstreamErrorEndsTheStream — a failed answer must not look complete: the
// reader fails rather than closing with the done marker.
func TestAnUpstreamErrorEndsTheStream(t *testing.T) {
	p := newTestProvider(t)
	body := p.Transcode(io.NopCloser(strings.NewReader(`data: {"error":{"code":500,"message":"backend down","status":"INTERNAL"}}`+"\n\n")), "gemini-3.8-flash")
	raw, err := io.ReadAll(body)
	if err == nil {
		t.Fatalf("the reader returned no error, only %q", string(raw))
	}
	if strings.Contains(string(raw), "[DONE]") {
		t.Fatal("the failed stream was closed with a done marker")
	}
}

// TestABodyWithoutEventsIsNotAnEmptyAnswer — a shape this route never promised
// must not be presented to the client as a model that said nothing.
func TestABodyWithoutEventsIsNotAnEmptyAnswer(t *testing.T) {
	p := newTestProvider(t)
	body := p.Transcode(io.NopCloser(strings.NewReader(`{"unexpected":true}`)), "gemini-3.8-flash")
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("a body without events was read as an answer")
	}
}
