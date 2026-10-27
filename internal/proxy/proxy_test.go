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
	"codex-pass/internal/requestlog"
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

func TestStreamingWrapsStringInputAndLogsDiff(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-6-luna","stream":true,"store":false,"input":"ping"}`)
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_input\",\"status\":\"completed\",\"output\":[],\"usage\":{}}}\n\n"))
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
	assertStringInputWrappedAndFieldsPreserved(t, requestBody, received)
	cleanup()
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logData, []byte(`"path":"$.input","before":"ping"`)) {
		t.Fatalf("string input conversion was not logged: %s", logData)
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
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", recorder)
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
	completion := recorder.waitForCompletion(t)
	if completion.Outcome != requestlog.OutcomeCanceled || completion.ErrorKind != "client_canceled" {
		t.Fatalf("cancellation completion = %#v", completion)
	}
}

func TestRequestLogCapturesTransparentStreamingUsageWithoutDebug(t *testing.T) {
	completed := `{"id":"resp_usage","status":"completed","model":"gpt-6.1-sol","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens":30,"cached_tokens":40},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":10}}}`
	expected := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write(expected)
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", recorder)
	defer cleanup()

	response := post(t, proxyServer, []byte(`{"model":"requested-model","stream":true,"reasoning":{"effort":"high"},"input":[]}`))
	actual, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("stream body changed:\n%s\nwant:\n%s", actual, expected)
	}
	requestRecord, completion := recorder.waitForRequestAndCompletion(t)
	if requestRecord.Endpoint != "responses" || requestRecord.Model != "requested-model" || requestRecord.ReasoningEffort != "high" || requestRecord.StartedAt.IsZero() {
		t.Fatalf("request record = %#v", requestRecord)
	}
	if completion.FinishedAt.IsZero() || completion.Outcome != requestlog.OutcomeSuccess || completion.Model != "gpt-6.1-sol" || completion.ReasoningEffort != "high" {
		t.Fatalf("completion = %#v", completion)
	}
	assertCompletionTokens(t, completion, 100, 30, 40, 20, 10)
}

func TestRequestLogStartsBeforeRequestBodyRead(t *testing.T) {
	completed := `{"id":"resp_slow_body","status":"completed","model":"gpt-6-luna","output":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n"))
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", recorder)
	defer cleanup()

	bodyReader, bodyWriter := io.Pipe()
	defer bodyWriter.Close()
	request, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/responses", bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-local-secret")
	request.Header.Set("Content-Type", "application/json")
	responseResult := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, requestErr := proxyServer.Client().Do(request)
		responseResult <- struct {
			response *http.Response
			err      error
		}{response: response, err: requestErr}
	}()

	requestRecord := recorder.waitForRequestStart(t)
	if requestRecord.Endpoint != "responses" || requestRecord.StartedAt.IsZero() || requestRecord.Model != "" || requestRecord.ReasoningEffort != "" {
		t.Fatalf("initial request record = %#v", requestRecord)
	}
	if _, err := bodyWriter.Write([]byte(`{"model":"requested-model","stream":true,"reasoning":{"effort":"high"},"input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := bodyWriter.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-responseResult
	if result.err != nil {
		t.Fatal(result.err)
	}
	if _, err := io.ReadAll(result.response.Body); err != nil {
		result.response.Body.Close()
		t.Fatal(err)
	}
	result.response.Body.Close()
	completion := recorder.waitForCompletion(t)
	if completion.Outcome != requestlog.OutcomeSuccess || completion.Model != "gpt-6-luna" || completion.ReasoningEffort != "high" {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestRequestLogForcesIdentityForTransparentStreaming(t *testing.T) {
	completed := `{"id":"resp_identity","status":"completed","model":"gpt-6-luna","output":[],"usage":{"input_tokens":5}}`
	expected := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n")
	upstreamEncoding := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamEncoding <- request.Header.Get("Accept-Encoding")
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write(expected)
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", recorder)
	defer cleanup()

	request, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-luna","stream":true,"input":[]}`))
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
	actual, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := <-upstreamEncoding; got != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q, want identity", got)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("stream body changed:\n%s\nwant:\n%s", actual, expected)
	}
	completion := recorder.waitForCompletion(t)
	if completion.Outcome != requestlog.OutcomeSuccess || completion.InputTokens == nil || *completion.InputTokens != 5 {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestRequestLogMarksIncompleteSSEAsFailure(t *testing.T) {
	terminal := `{"status":"incomplete","model":"gpt-6-luna","usage":{"input_tokens":8,"output_tokens":2}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":" + terminal + "}\n\n"))
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", recorder)
	defer cleanup()

	response := post(t, proxyServer, []byte(`{"model":"gpt-6-luna","input":[]}`))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	completion := recorder.waitForCompletion(t)
	if completion.Outcome != requestlog.OutcomeFailure || completion.ErrorKind != "stream_aggregation" {
		t.Fatalf("failure completion = %#v", completion)
	}
	if completion.InputTokens == nil || *completion.InputTokens != 8 || completion.OutputTokens == nil || *completion.OutputTokens != 2 {
		t.Fatalf("terminal usage was not recorded: %#v", completion)
	}
}

func TestRequestLogMarksTransparentTerminalEventAsFailure(t *testing.T) {
	terminal := `{"model":"gpt-6-luna","usage":{"input_tokens":8,"output_tokens":2}}`
	expected := []byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":" + terminal + "}\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write(expected)
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", recorder)
	defer cleanup()

	response := post(t, proxyServer, []byte(`{"model":"gpt-6-luna","stream":true,"input":[]}`))
	actual, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("transparent terminal body changed: %s", actual)
	}
	completion := recorder.waitForCompletion(t)
	if completion.Outcome != requestlog.OutcomeFailure || completion.ErrorKind != "upstream_terminal" {
		t.Fatalf("transparent terminal completion = %#v", completion)
	}
	if completion.InputTokens == nil || *completion.InputTokens != 8 || completion.OutputTokens == nil || *completion.OutputTokens != 2 {
		t.Fatalf("transparent terminal usage was not recorded: %#v", completion)
	}
}

func TestSQLiteRequestLogIntegratesWithProxyAndSummary(t *testing.T) {
	completed := `{"id":"resp_sqlite","status":"completed","model":"gpt-6-luna","output":[],"usage":{"input_tokens":9,"output_tokens":2}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n"))
	}))
	defer upstream.Close()
	store, err := requestlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "", store)
	defer cleanup()

	response := post(t, proxyServer, []byte(`{"model":"gpt-6-luna","stream":true,"input":[]}`))
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	summary, err := store.Summary(requestlog.TimeRange{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total.Successes != 1 || summary.Total.InputTokens != 9 || summary.Total.OutputTokens != 2 {
		t.Fatalf("SQLite summary = %#v", summary.Total)
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

func newTestProxy(t *testing.T, upstream *httptest.Server, refreshEndpoint, debugPath string, requestLogs ...requestlog.Recorder) (*httptest.Server, func()) {
	return newTestProxyConfigured(t, upstream, refreshEndpoint, debugPath, nil, requestLogs...)
}

func newTestProxyConfigured(t *testing.T, upstream *httptest.Server, refreshEndpoint, debugPath string, configure func(*Config), requestLogs ...requestlog.Recorder) (*httptest.Server, func()) {
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
	var requestLog requestlog.Recorder
	if len(requestLogs) > 1 {
		t.Fatal("only one request log recorder may be provided")
	}
	if len(requestLogs) == 1 {
		requestLog = requestLogs[0]
	}
	config := Config{APIKey: "sk-local-secret", AuthPool: pool, HTTPClient: upstream.Client(), UpstreamURL: upstream.URL + "/responses", ModelsUpstreamURL: upstream.URL + "/models", CodexUpstreamURL: upstream.URL, Logger: logger, RequestLog: requestLog}
	if configure != nil {
		configure(&config)
	}
	handler, err := New(config)
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

type captureRecorder struct {
	mu          sync.Mutex
	requests    []requestlog.Request
	completions []requestlog.Completion
	begun       chan struct{}
	finished    chan struct{}
}

func newCaptureRecorder() *captureRecorder {
	return &captureRecorder{begun: make(chan struct{}, 16), finished: make(chan struct{}, 16)}
}

func (r *captureRecorder) Begin(request requestlog.Request) error {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	r.begun <- struct{}{}
	return nil
}

func (r *captureRecorder) UpdateMetadata(metadata requestlog.RequestMetadata) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.requests {
		if r.requests[index].RequestID != metadata.RequestID {
			continue
		}
		if metadata.Model != "" {
			r.requests[index].Model = metadata.Model
		}
		if metadata.ReasoningEffort != "" {
			r.requests[index].ReasoningEffort = metadata.ReasoningEffort
		}
		return nil
	}
	return fmt.Errorf("unknown request ID %q", metadata.RequestID)
}

func (r *captureRecorder) Finish(completion requestlog.Completion) error {
	r.mu.Lock()
	r.completions = append(r.completions, completion)
	r.mu.Unlock()
	r.finished <- struct{}{}
	return nil
}

func (r *captureRecorder) Err() error { return nil }

func (r *captureRecorder) waitForRequestStart(t *testing.T) requestlog.Request {
	t.Helper()
	select {
	case <-r.begun:
	case <-time.After(2 * time.Second):
		t.Fatal("request log start was not recorded")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(r.requests))
	}
	return r.requests[0]
}

func (r *captureRecorder) waitForCompletion(t *testing.T) requestlog.Completion {
	t.Helper()
	select {
	case <-r.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("request log completion was not recorded")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.completions) != 1 {
		t.Fatalf("completion count = %d, want 1", len(r.completions))
	}
	return r.completions[0]
}

func (r *captureRecorder) waitForRequestAndCompletion(t *testing.T) (requestlog.Request, requestlog.Completion) {
	completion := r.waitForCompletion(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(r.requests))
	}
	return r.requests[0], completion
}

func assertCompletionTokens(t *testing.T, completion requestlog.Completion, input, cacheWrite, cacheRead, output, reasoning int64) {
	t.Helper()
	if completion.InputTokens == nil || *completion.InputTokens != input ||
		completion.CacheWriteTokens == nil || *completion.CacheWriteTokens != cacheWrite ||
		completion.CacheReadTokens == nil || *completion.CacheReadTokens != cacheRead ||
		completion.OutputTokens == nil || *completion.OutputTokens != output ||
		completion.ReasoningTokens == nil || *completion.ReasoningTokens != reasoning {
		t.Fatalf("completion token accounting = %#v", completion)
	}
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

func assertStringInputWrappedAndFieldsPreserved(t *testing.T, before, after []byte) {
	t.Helper()
	var original, upstream map[string]any
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &upstream); err != nil {
		t.Fatal(err)
	}
	original["input"] = []any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": "ping",
		}},
	}}
	if !reflect.DeepEqual(original, upstream) {
		t.Fatalf("string input conversion changed other fields\nbefore=%s\nafter=%s", before, after)
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
