package proxy

import (
	"strings"
	"testing"

	"codex-pass/internal/sse"
)

func TestRequestLogRequestMetadataReadsResponsesAndChatEffort(t *testing.T) {
	model, effort := requestLogRequestMetadata([]byte(`{"model":"gpt-6-luna","reasoning":{"effort":"high"},"reasoning_effort":"low"}`))
	if model != "gpt-6-luna" || effort != "high" {
		t.Fatalf("Responses metadata = model %q effort %q", model, effort)
	}
	model, effort = requestLogRequestMetadata([]byte(`{"model":"gpt-6.1-sol","reasoning_effort":"medium"}`))
	if model != "gpt-6.1-sol" || effort != "medium" {
		t.Fatalf("Chat metadata = model %q effort %q", model, effort)
	}
}

func TestResponseAccountingPreservesZeroTokenCounts(t *testing.T) {
	accounting, valid := responseAccounting([]byte(`{
		"model":"gpt-6-luna",
		"status":"completed",
		"usage":{
			"input_tokens":0,
			"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},
			"output_tokens":0,
			"output_tokens_details":{"reasoning_tokens":0}
		}
	}`))
	if !valid || accounting.Model != "gpt-6-luna" || accounting.Status != "completed" {
		t.Fatalf("accounting metadata = %#v, valid=%t", accounting, valid)
	}
	for name, value := range map[string]*int64{
		"input":       accounting.InputTokens,
		"cache write": accounting.CacheWriteTokens,
		"cache read":  accounting.CacheReadTokens,
		"output":      accounting.OutputTokens,
		"reasoning":   accounting.ReasoningTokens,
	} {
		if value == nil || *value != 0 {
			t.Fatalf("%s tokens = %v, want pointer to 0", name, value)
		}
	}
}

func TestStreamTraceSkipsOversizedEvent(t *testing.T) {
	terminal := `{"id":"resp_after_large","status":"completed"}`
	stream := "event: response.output_text.delta\n" +
		"data: " + strings.Repeat("x", 2048) + "\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":" + terminal + "}\n\n"
	trace := &streamTrace{}
	trace.consume(strings.NewReader(stream), 512, func(sse.Event) {})
	response, kind := trace.terminalResponse()
	if kind != "response.completed" || string(response) != terminal {
		t.Fatalf("terminal response = %q kind = %q", response, kind)
	}
}
