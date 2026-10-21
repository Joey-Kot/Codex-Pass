package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-pass/internal/auth"
	debuglog "codex-pass/internal/debug"
)

const transparentRequest = `{
  "model": "gpt-6-luna",
  "stream": true,
  "max_output_tokens": 128000,
  "service_tier": "priority",
  "store": false,
  "prompt_cache_key": "fixed-key",
  "input": [{"role": "developer", "content": [{"type": "input_text", "text": "Testing"}]}]
}`

func TestStreamingRequestAndSSEAreByteTransparent(t *testing.T) {
	requestBody := []byte(transparentRequest)
	expectedSSE := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1}}}\n\n")
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var err error
		received, err = io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-access" {
			t.Errorf("upstream authorization = %q", got)
		}
		if got := request.Header.Get("ChatGPT-Account-ID"); got != "test-account" {
			t.Errorf("account header = %q", got)
		}
		if got := request.Header.Get("session-id"); got != "fixed-key" {
			t.Errorf("session-id = %q", got)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("X-Upstream", "kept")
		_, _ = writer.Write(expectedSSE[:31])
		if flush, ok := writer.(http.Flusher); ok {
			flush.Flush()
		}
		_, _ = writer.Write(expectedSSE[31:])
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()

	response := post(t, proxyServer, requestBody)
	defer response.Body.Close()
	actualSSE, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Upstream") != "kept" {
		t.Fatalf("unexpected proxy response: status=%d headers=%v", response.StatusCode, response.Header)
	}
	if sha256.Sum256(received) != sha256.Sum256(requestBody) {
		t.Fatalf("stream request body changed\nreceived=%s", received)
	}
	if sha256.Sum256(actualSSE) != sha256.Sum256(expectedSSE) {
		t.Fatalf("stream SSE body changed\nreceived=%q\nwant=%q", actualSSE, expectedSSE)
	}
	assertTransparentFields(t, received)
}

func TestStreamingFastServiceTierMapsToPriorityAndLogsDiff(t *testing.T) {
	requestBody := []byte(strings.Replace(transparentRequest, `"service_tier": "priority"`, `"service_tier": "fast"`, 1))
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fast\",\"status\":\"completed\",\"output\":[],\"usage\":{}}}\n\n"))
	}))
	defer upstream.Close()
	logPath := filepath.Join(t.TempDir(), "proxy.jsonl")
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", logPath)
	response := post(t, proxyServer, requestBody)
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	assertOnlyServiceTierChanged(t, requestBody, received)
	cleanup()
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logData, []byte(`"path":"$.service_tier","before":"fast","after":"priority"`)) {
		t.Fatalf("service tier mapping was not logged: %s", logData)
	}
}

func TestStreamingForcesStoreFalseAndLogsDiff(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		log  string
	}{
		{
			name: "missing store",
			body: strings.Replace(transparentRequest, `  "store": false,`+"\n", "", 1),
			log:  `"added_fields":[{"path":"$.store","before":null,"after":false}]`,
		},
		{
			name: "true store",
			body: strings.Replace(transparentRequest, `"store": false`, `"store": true`, 1),
			log:  `"path":"$.store","before":true,"after":false`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var received []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				received, _ = io.ReadAll(request.Body)
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_store\",\"status\":\"completed\",\"output\":[],\"usage\":{}}}\n\n"))
			}))
			defer upstream.Close()
			logPath := filepath.Join(t.TempDir(), "proxy.jsonl")
			proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", logPath)
			response := post(t, proxyServer, []byte(test.body))
			defer response.Body.Close()
			if _, err := io.ReadAll(response.Body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			assertStoreFalseAndFieldsPreserved(t, []byte(test.body), received)
			cleanup()
			logData, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(logData, []byte(test.log)) {
				t.Fatalf("store mapping was not logged: %s", logData)
			}
		})
	}
}

func TestStreamingRemovesTemperatureAndLogsDiff(t *testing.T) {
	requestBody := []byte(strings.Replace(transparentRequest, `  "max_output_tokens": 128000,`, "  \"temperature\": 0,\n  \"max_output_tokens\": 128000,", 1))
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_temperature\",\"status\":\"completed\",\"output\":[],\"usage\":{}}}\n\n"))
	}))
	defer upstream.Close()
	logPath := filepath.Join(t.TempDir(), "proxy.jsonl")
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", logPath)
	response := post(t, proxyServer, requestBody)
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	assertTemperatureRemovedAndFieldsPreserved(t, requestBody, received)
	cleanup()
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logData, []byte(`"removed_fields":[{"path":"$.temperature","before":0,"after":null}]`)) {
		t.Fatalf("temperature removal was not logged: %s", logData)
	}
}

func TestStreamingRemovesPromptCacheControlsAndLogsDiff(t *testing.T) {
	requestBody := []byte(strings.Replace(
		strings.Replace(transparentRequest, `  "prompt_cache_key": "fixed-key",`, "  \"prompt_cache_key\": \"fixed-key\",\n  \"prompt_cache_options\": {\"mode\": \"explicit\", \"ttl\": \"30m\"},", 1),
		`"text": "Testing"`, `"text": "Testing", "prompt_cache_breakpoint": {"mode": "explicit"}`, 1,
	))
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cache\",\"status\":\"completed\",\"output\":[],\"usage\":{}}}\n\n"))
	}))
	defer upstream.Close()
	logPath := filepath.Join(t.TempDir(), "proxy.jsonl")
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", logPath)
	response := post(t, proxyServer, requestBody)
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	assertCacheControlsRemovedAndFieldsPreserved(t, requestBody, received)
	cleanup()
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"$.prompt_cache_options", "$.input[0].content[0].prompt_cache_breakpoint"} {
		if !bytes.Contains(logData, []byte(`"path":"`+path+`"`)) {
			t.Fatalf("cache control removal for %s was not logged: %s", path, logData)
		}
	}
}

func TestNonStreamingChangesOnlyStreamAndAggregatesFinalResponse(t *testing.T) {
	requestBody := []byte(strings.Replace(transparentRequest, `"stream": true`, `"stream": false`, 1))
	var upstreamBody []byte
	completed := `{"id":"resp_nonstream","status":"completed","model":"gpt-6-luna","service_tier":"fast","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":77,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":null}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer, "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\r\n\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":%s}\r\n\r\n", completed)
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()

	response := post(t, proxyServer, requestBody)
	defer response.Body.Close()
	actual, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, actual)
	}
	assertOnlyStreamChanged(t, requestBody, upstreamBody)
	assertTransparentFields(t, upstreamBody)
	if !jsonEqual(actual, []byte(completed)) {
		t.Fatalf("final response changed\ngot=%s\nwant=%s", actual, completed)
	}
}

func TestUpstreamErrorsAreTransparent(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			payload := fmt.Sprintf(`{"error":{"status":%d,"message":"upstream"}}`, status)
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(status)
				_, _ = writer.Write([]byte(payload))
			}))
			defer upstream.Close()
			proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
			defer cleanup()
			response := post(t, proxyServer, []byte(strings.Replace(transparentRequest, `"stream": true`, `"stream": false`, 1)))
			defer response.Body.Close()
			actual, _ := io.ReadAll(response.Body)
			if response.StatusCode != status || string(actual) != payload {
				t.Fatalf("got status=%d body=%s, want status=%d body=%s", response.StatusCode, actual, status, payload)
			}
		})
	}
}

func TestUnauthorizedRefreshesOnceAndReturnsSecondUpstreamResult(t *testing.T) {
	var calls atomic.Int32
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/refresh":
			refreshCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`))
		case "/responses":
			call := calls.Add(1)
			if call == 1 && request.Header.Get("Authorization") != "Bearer test-access" {
				t.Errorf("first authorization = %q", request.Header.Get("Authorization"))
			}
			if call == 2 && request.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Errorf("second authorization = %q", request.Header.Get("Authorization"))
			}
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":"still unauthorized"}`))
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()
	response := post(t, proxyServer, []byte(`{"model":"gpt-6-luna","stream":true,"input":[]}`))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusUnauthorized || string(body) != `{"error":"still unauthorized"}` {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if calls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("response calls=%d refresh calls=%d", calls.Load(), refreshCalls.Load())
	}
}

func TestConcurrentNonStreamingRequestsAndDebugCorrelation(t *testing.T) {
	var received atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var input map[string]any
		if err := json.Unmarshal(body, &input); err != nil {
			t.Error(err)
		}
		id := input["metadata"].(map[string]any)["id"].(string)
		received.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1}}}\n\n", id)
	}))
	defer upstream.Close()
	logPath := filepath.Join(t.TempDir(), "proxy.jsonl")
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", logPath)
	var group sync.WaitGroup
	errs := make(chan error, 100)
	for index := range 100 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			id := fmt.Sprintf("request-%03d", index)
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-luna","stream":false,"input":[],"metadata":{"id":%q}}`, id))
			response := post(t, proxyServer, body)
			defer response.Body.Close()
			actual, err := io.ReadAll(response.Body)
			if err != nil {
				errs <- err
				return
			}
			if response.StatusCode != http.StatusOK || !bytes.Contains(actual, []byte(id)) {
				errs <- fmt.Errorf("id %s: status=%d body=%s", id, response.StatusCode, actual)
			}
		}(index)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	cleanup()
	if got := received.Load(); got != 100 {
		t.Fatalf("upstream received %d requests", got)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(logData, []byte(`"stage":"REQUEST_SUMMARY"`)); got != 100 {
		t.Fatalf("summary records=%d, want 100", got)
	}
}

func TestClientCancellationCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
		writer.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()
	context, cancel := ContextWithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(context, http.MethodPost, proxyServer.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-luna","stream":true,"input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-local-secret")
	response, err := proxyServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not cancelled")
	}
}

func TestDiffJSONReportsOnlyTopLevelStreamChange(t *testing.T) {
	before := []byte(`{"stream":false,"unknown":{"keep":[1,true]}}`)
	after := []byte(`{"stream":true,"unknown":{"keep":[1,true]}}`)
	diff, err := DiffJSON(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.ChangedFields) != 1 || diff.ChangedFields[0].Path != "$.stream" || len(diff.AddedFields) != 0 || len(diff.RemovedFields) != 0 {
		t.Fatalf("unexpected diff: %#v", diff)
	}
}

func newTestProxy(t *testing.T, upstream *httptest.Server, refreshEndpoint, debugPath string) (*httptest.Server, func()) {
	t.Helper()
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"test-access","refresh_token":"test-refresh","account_id":"test-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credential, err := auth.Load(authPath)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := auth.NewPool([]*auth.Credential{credential}, auth.RefreshConfig{Endpoint: refreshEndpoint, Client: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var logger *debuglog.Logger
	if debugPath != "" {
		logger, err = debuglog.New(debugPath)
		if err != nil {
			t.Fatal(err)
		}
	}
	handler, err := New(Config{APIKey: "sk-local-secret", AuthPool: pool, HTTPClient: upstream.Client(), UpstreamURL: upstream.URL + "/responses", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			server.Close()
			if logger != nil {
				if err := logger.Close(); err != nil {
					t.Errorf("close logger: %v", err)
				}
			}
		})
	}
	return server, cleanup
}

func post(t *testing.T, server *httptest.Server, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body))
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

func assertTransparentFields(t *testing.T, body []byte) {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if value["service_tier"] != "priority" || value["max_output_tokens"].(float64) != 128000 {
		t.Fatalf("model fields changed: %s", body)
	}
	content := value["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["text"] != "Testing" {
		t.Fatalf("input content changed: %s", body)
	}
	if _, found := value["tools"]; found {
		t.Fatalf("unexpected tools injected: %s", body)
	}
}

func assertOnlyServiceTierChanged(t *testing.T, before, after []byte) {
	t.Helper()
	var original, upstream map[string]any
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &upstream); err != nil {
		t.Fatal(err)
	}
	original["service_tier"] = "priority"
	if !reflect.DeepEqual(original, upstream) {
		t.Fatalf("fast tier mapping changed more than service_tier\nbefore=%s\nafter=%s", before, after)
	}
}

func assertStoreFalseAndFieldsPreserved(t *testing.T, before, after []byte) {
	t.Helper()
	var original, upstream map[string]any
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &upstream); err != nil {
		t.Fatal(err)
	}
	original["store"] = false
	if !reflect.DeepEqual(original, upstream) {
		t.Fatalf("store mapping changed more than store\nbefore=%s\nafter=%s", before, after)
	}
}

func assertTemperatureRemovedAndFieldsPreserved(t *testing.T, before, after []byte) {
	t.Helper()
	var original, upstream map[string]any
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &upstream); err != nil {
		t.Fatal(err)
	}
	delete(original, "temperature")
	if !reflect.DeepEqual(original, upstream) {
		t.Fatalf("temperature removal changed more than temperature\nbefore=%s\nafter=%s", before, after)
	}
}

func assertCacheControlsRemovedAndFieldsPreserved(t *testing.T, before, after []byte) {
	t.Helper()
	var original, upstream map[string]any
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &upstream); err != nil {
		t.Fatal(err)
	}
	delete(original, "prompt_cache_options")
	content := original["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	delete(content, "prompt_cache_breakpoint")
	if !reflect.DeepEqual(original, upstream) {
		t.Fatalf("cache control removal changed other fields\nbefore=%s\nafter=%s", before, after)
	}
}

func assertOnlyStreamChanged(t *testing.T, before, after []byte) {
	t.Helper()
	var original, upstream map[string]any
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &upstream); err != nil {
		t.Fatal(err)
	}
	original["stream"] = true
	if !reflect.DeepEqual(original, upstream) {
		t.Fatalf("non-stream request changed more than stream\nbefore=%s\nafter=%s", before, after)
	}
}

func jsonEqual(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}
