package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-pass/internal/requestlog"
	"codex-pass/internal/sse"
)

func TestChatCompletionNonStreamingConvertsResponsesResult(t *testing.T) {
	var upstreamBody []byte
	completed := `{"id":"resp_chat","created_at":1710000000,"model":"gpt-6-luna","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello "},{"type":"output_text","text":"world"}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n"))
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()

	response := postChat(t, proxyServer, []byte(`{
  "model":"gpt-6-luna",
  "max_completion_tokens":12,
  "messages":[
    {"role":"system","content":"Be concise."},
    {"role":"user","content":"Say hello."}
  ]
}`))
	defer response.Body.Close()
	actual, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, actual)
	}

	var upstreamRequest map[string]any
	if err := json.Unmarshal(upstreamBody, &upstreamRequest); err != nil {
		t.Fatal(err)
	}
	if upstreamRequest["stream"] != true || upstreamRequest["max_output_tokens"].(float64) != 12 || upstreamRequest["store"] != false {
		t.Fatalf("unexpected converted Responses request: %s", upstreamBody)
	}
	input := upstreamRequest["input"].([]any)
	if len(input) != 2 || input[0].(map[string]any)["role"] != "developer" || input[1].(map[string]any)["role"] != "user" {
		t.Fatalf("unexpected converted input: %s", upstreamBody)
	}
	if text := input[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "Be concise." {
		t.Fatalf("system content was not preserved: %s", upstreamBody)
	}

	var completion map[string]any
	if err := json.Unmarshal(actual, &completion); err != nil {
		t.Fatal(err)
	}
	if completion["id"] != "resp_chat" || completion["object"] != "chat.completion" || completion["created"].(float64) != 1710000000 || completion["model"] != "gpt-6-luna" {
		t.Fatalf("unexpected chat completion envelope: %s", actual)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] != "Hello world" || choice["finish_reason"] != "stop" {
		t.Fatalf("unexpected chat completion choice: %s", actual)
	}
	usage := completion["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 7 || usage["completion_tokens"].(float64) != 3 || usage["total_tokens"].(float64) != 10 {
		t.Fatalf("unexpected mapped usage: %s", actual)
	}
}

func TestChatCompletionRequestLogCapturesUsage(t *testing.T) {
	completed := `{"id":"resp_chat_usage","created_at":1710000000,"model":"gpt-6.1-sol","status":"completed","output":[],"usage":{"input_tokens":12,"input_tokens_details":{"cache_write_tokens":3,"cached_tokens":4},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":6}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n"))
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", recorder)
	defer cleanup()

	response := postChat(t, proxyServer, []byte(`{"model":"requested-chat-model","reasoning":{"effort":"medium"},"messages":[{"role":"user","content":"hello"}]}`))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	requestRecord, completion := recorder.waitForRequestAndCompletion(t)
	if requestRecord.Endpoint != "chat_completions" || requestRecord.Model != "requested-chat-model" || requestRecord.ReasoningEffort != "medium" || requestRecord.StartedAt.IsZero() {
		t.Fatalf("request record = %#v", requestRecord)
	}
	if completion.Outcome != requestlog.OutcomeSuccess || completion.Model != "gpt-6.1-sol" || completion.ReasoningEffort != "medium" {
		t.Fatalf("completion = %#v", completion)
	}
	assertCompletionTokens(t, completion, 12, 3, 4, 5, 6)
}

func TestChatCompletionStreamingConvertsResponsesEvents(t *testing.T) {
	completed := `{"id":"resp_stream_chat","created_at":1710000001,"model":"gpt-6-luna","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello world"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			t.Errorf("Chat stream did not become an upstream Responses stream: %s", body)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream_chat\",\"created_at\":1710000001,\"model\":\"gpt-6-luna\"}}\n\n"))
		_, _ = writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n"))
		_, _ = writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"world\"}\n\n"))
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n"))
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()

	response := postChat(t, proxyServer, []byte(`{"model":"gpt-6-luna","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d content-type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
	parser := sse.NewParser(response.Body, 1<<20)
	var events []map[string]any
	for {
		event, err := parser.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(event.Data) == "[DONE]" {
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			t.Fatalf("invalid converted SSE event %s: %v", event.Data, err)
		}
		events = append(events, chunk)
	}
	if len(events) != 5 {
		t.Fatalf("converted event count=%d, want 5: %#v", len(events), events)
	}
	role := events[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if role["role"] != "assistant" {
		t.Fatalf("first event did not set the assistant role: %#v", events[0])
	}
	for index, expected := range []string{"Hello ", "world"} {
		delta := events[index+1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if delta["content"] != expected {
			t.Fatalf("content event %d = %#v, want %q", index, delta, expected)
		}
	}
	terminal := events[3]["choices"].([]any)[0].(map[string]any)
	if terminal["finish_reason"] != "stop" {
		t.Fatalf("missing stop event: %#v", terminal)
	}
	if choices := events[4]["choices"].([]any); len(choices) != 0 {
		t.Fatalf("usage event choices = %#v, want empty", choices)
	}
	usage := events[4]["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 4 || usage["completion_tokens"].(float64) != 2 || usage["total_tokens"].(float64) != 6 {
		t.Fatalf("stream usage conversion failed: %#v", usage)
	}
}

func TestResponseToChatCompletionMapsFunctionCalls(t *testing.T) {
	response := []byte(`{"id":"resp_tool","created_at":1710000002,"model":"gpt-6-luna","output":[{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"}]}`)
	converted, err := responseToChatCompletion(response, "")
	if err != nil {
		t.Fatal(err)
	}
	var completion map[string]any
	if err := json.Unmarshal(converted, &completion); err != nil {
		t.Fatal(err)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish reason = %#v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != nil {
		t.Fatalf("tool call message content = %#v, want null", message["content"])
	}
	call := message["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || call["function"].(map[string]any)["name"] != "weather" {
		t.Fatalf("unexpected converted tool call: %#v", call)
	}
}

func TestResponseToChatCompletionPreservesTextWithFunctionCalls(t *testing.T) {
	response := []byte(`{"id":"resp_tool_text","created_at":1710000003,"model":"gpt-6-luna","output":[{"type":"message","content":[{"type":"output_text","text":"I will look that up."}]},{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"}]}`)
	converted, err := responseToChatCompletion(response, "")
	if err != nil {
		t.Fatal(err)
	}
	var completion map[string]any
	if err := json.Unmarshal(converted, &completion); err != nil {
		t.Fatal(err)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "I will look that up." {
		t.Fatalf("text content was dropped: %#v", message)
	}
	if toolCalls := message["tool_calls"].([]any); len(toolCalls) != 1 || toolCalls[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("tool calls were not preserved: %#v", message)
	}
}

func TestChatCompletionForcesIdentityUpstreamEncoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("upstream Accept-Encoding = %q, want identity", got)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_identity\",\"model\":\"gpt-6-luna\",\"output\":[]}}\n\n"))
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()

	request, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-6-luna","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-local-secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := proxyServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}

func TestChatStreamingFunctionArgumentDeltaUsesItemID(t *testing.T) {
	converter := newChatStreamConverter("gpt-6-luna", false)
	_, _, _, err := converter.consume(sse.Event{Event: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"weather"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	chunks, _, _, err := converter.consume(sse.Event{Event: "response.function_call_arguments.delta", Data: []byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"city\":\"Paris\"}"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("argument chunks=%#v", chunks)
	}
	encoded, err := json.Marshal(chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	toolCalls := normalized["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)
	if toolCalls[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("argument chunk = %#v", chunks[0])
	}
}

func postChat(t *testing.T, server *httptest.Server, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-local-secret")
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
