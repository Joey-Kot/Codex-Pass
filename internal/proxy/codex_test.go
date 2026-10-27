package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-pass/internal/requestlog"
	"github.com/klauspost/compress/zstd"
)

const codexCompleted = `{"id":"resp_codex","model":"returned-model","status":"completed","output":[],"usage":{"input_tokens":13,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":2},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":20}}`
const codexSSE = "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + codexCompleted + "}\n\n"

func enableCodex(config *Config) { config.EnableCodex = true }

func encodeCodexTestBody(t *testing.T, body []byte, encoding string) []byte {
	t.Helper()
	switch encoding {
	case "zstd":
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer encoder.Close()
		return encoder.EncodeAll(body, nil)
	case "gzip":
		var buffer bytes.Buffer
		encoder := gzip.NewWriter(&buffer)
		if _, err := encoder.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	default:
		return body
	}
}

func codexRequest(t *testing.T, proxyServer *httptest.Server, body []byte, encoding string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/codex/responses?scope=a%2Fb&scope=c", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer client-chatgpt-token")
	request.Header.Set("ChatGPT-Account-ID", "client-account")
	request.Header.Set("Originator", "codex_cli_rs")
	request.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		request.Header.Set("Content-Encoding", encoding)
	}
	response, err := proxyServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestCodexEntryIsOptInAndDoesNotBypassV1Authentication(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/models" {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, codexSSE)
	}))
	defer upstream.Close()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", func(c *Config) { c.EnableCodex = enabled })
			defer cleanup()
			codexStatus := http.StatusNotFound
			if enabled {
				codexStatus = http.StatusOK
			}
			for _, test := range []struct {
				method, path, token string
				want                int
			}{
				{http.MethodGet, "/codex/models", "", codexStatus},
				{http.MethodPost, "/codex/responses", "client-chatgpt-token", codexStatus},
				{http.MethodGet, "/v1/models", "", http.StatusUnauthorized},
				{http.MethodGet, "/v1/models", "client-chatgpt-token", http.StatusUnauthorized},
				{http.MethodPost, "/v1/responses", "client-chatgpt-token", http.StatusUnauthorized},
				{http.MethodPost, "/v1/chat/completions", "client-chatgpt-token", http.StatusUnauthorized},
				{http.MethodGet, "/v1/models", "sk-local-secret", http.StatusOK},
				{http.MethodPost, "/codex/chat/completions", "", http.StatusNotFound},
				{http.MethodGet, "/codex/../v1/models", "", http.StatusNotFound},
			} {
				request, err := http.NewRequest(test.method, server.URL+test.path, strings.NewReader(`{"stream":true,"store":false}`))
				if err != nil {
					t.Fatal(err)
				}
				if test.token != "" {
					request.Header.Set("Authorization", "Bearer "+test.token)
				}
				before := calls.Load()
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != test.want {
					t.Fatalf("%s %s: status=%d want=%d", test.method, test.path, response.StatusCode, test.want)
				}
				if test.want != http.StatusOK && calls.Load() != before {
					t.Fatalf("rejected request reached upstream: %s", test.path)
				}
			}
			if enabled {
				upgrade, _ := http.NewRequest(http.MethodGet, server.URL+"/codex/responses", nil)
				upgrade.Header.Set("Connection", "Upgrade")
				upgrade.Header.Set("Upgrade", "websocket")
				response, err := server.Client().Do(upgrade)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusUpgradeRequired {
					t.Fatalf("WebSocket fallback status=%d", response.StatusCode)
				}
				for _, path := range []string{"/codex/models", "/codex/responses"} {
					request, _ := http.NewRequest(http.MethodDelete, server.URL+path, nil)
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") == "" {
						t.Fatalf("wrong-method response: %d %v", response.StatusCode, response.Header)
					}
				}
			}
		})
	}
}

func TestCodexBodyRemainsUnchangedWithCompressionAndAccounting(t *testing.T) {
	// These fields deliberately exercise every rewrite on the generic endpoint.
	body := []byte("{\n  \"model\":\"requested-model\",\"reasoning\":{\"effort\":\"high\"},\"stream\":true,\"store\":true,\"input\":\"ping\",\"service_tier\":\"fast\",\"temperature\":0.75,\"prompt_cache_key\":\"native-cache\",\"prompt_cache_options\":{\"x\":true},\"prompt_cache_breakpoint\":true,\"future\":{\"integer\":9007199254740993,\"prompt_cache_breakpoint\":true}\n}\n")
	for _, encoding := range []string{"", "zstd", "gzip"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			wire := encodeCodexTestBody(t, body, encoding)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, wire) {
					t.Error("Codex request wire bytes changed")
				}
				if r.URL.RawQuery != "scope=a%2Fb&scope=c" {
					t.Errorf("query=%q", r.URL.RawQuery)
				}
				for key, want := range map[string]string{"Authorization": "Bearer test-access", "ChatGPT-Account-ID": "test-account", "Content-Encoding": encoding, "Originator": "codex_cli_rs", "Session-ID": "native-cache", "Accept-Encoding": "identity"} {
					if got := r.Header.Get(key); got != want {
						t.Errorf("%s=%q want=%q", key, got, want)
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Upstream", "preserved")
				_, _ = io.WriteString(w, codexSSE)
			}))
			defer upstream.Close()
			recorder := newCaptureRecorder()
			logPath := filepath.Join(t.TempDir(), "debug.jsonl")
			server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", logPath, enableCodex, recorder)
			defer cleanup()
			response := codexRequest(t, server, wire, encoding)
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || string(got) != codexSSE || response.Header.Get("X-Upstream") != "preserved" {
				t.Fatalf("response changed: status=%d body=%s", response.StatusCode, got)
			}
			completion := recorder.waitForCompletion(t)
			request := recorder.waitForRequestStart(t)
			if request.Endpoint != "codex/responses" || request.Model != "requested-model" || request.ReasoningEffort != "high" {
				t.Fatalf("request metadata=%+v", request)
			}
			if completion.Outcome != requestlog.OutcomeSuccess || completion.Model != "returned-model" || completion.InputTokens == nil || *completion.InputTokens != 13 || completion.CacheReadTokens == nil || *completion.CacheReadTokens != 5 || completion.CacheWriteTokens == nil || *completion.CacheWriteTokens != 2 || completion.OutputTokens == nil || *completion.OutputTokens != 7 || completion.ReasoningTokens == nil || *completion.ReasoningTokens != 3 {
				t.Fatalf("completion=%+v", completion)
			}
			cleanup()
			logs, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(logs, []byte(`"changed_fields":[],"removed_fields":[],"added_fields":[]`)) || !bytes.Contains(logs, []byte(`\"prompt_cache_breakpoint\":true`)) || !bytes.Contains(logs, []byte(`"usage"`)) {
				t.Fatalf("missing native debug records: %s", logs)
			}
			for _, secret := range []string{"client-chatgpt-token", "test-access", "sk-local-secret"} {
				if bytes.Contains(logs, []byte(secret)) {
					t.Fatalf("debug log contains %q", secret)
				}
			}
		})
	}
}

func TestCodexRetryPreservesCompressedRequest(t *testing.T) {
	wire := encodeCodexTestBody(t, []byte(`{"model":"native-model","stream":true}`), "zstd")
	var requests, refreshes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			refreshes.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
			return
		}
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, wire) || r.Header.Get("Content-Encoding") != "zstd" || r.URL.RawQuery != "scope=a%2Fb&scope=c" {
			t.Error("retry changed the request")
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-access" {
			t.Error("retry did not use refreshed credentials")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, codexSSE)
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", enableCodex, recorder)
	defer cleanup()
	response := codexRequest(t, server, wire, "zstd")
	_, err := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || requests.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("retry: status=%d calls=%d refreshes=%d error=%v", response.StatusCode, requests.Load(), refreshes.Load(), err)
	}
	if completion := recorder.waitForCompletion(t); completion.Outcome != requestlog.OutcomeSuccess {
		t.Fatalf("completion=%+v", completion)
	}
}

func TestCodexRejectsInvalidOrOversizedInspectionCopies(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", func(c *Config) { c.EnableCodex = true; c.MaxBodyBytes = 1024 })
	defer cleanup()
	for _, test := range []struct {
		name, encoding string
		body           []byte
		want           int
	}{
		{"invalid JSON", "", []byte("broken"), http.StatusBadRequest},
		{"non-object JSON", "", []byte("[]"), http.StatusBadRequest},
		{"invalid stream", "", []byte(`{"stream":"yes"}`), http.StatusBadRequest},
		{"invalid zstd", "zstd", []byte("broken"), http.StatusBadRequest},
		{"invalid gzip", "gzip", []byte("broken"), http.StatusBadRequest},
		{"unsupported encoding", "br", []byte("broken"), http.StatusUnsupportedMediaType},
		{"decoded limit", "zstd", encodeCodexTestBody(t, []byte(strings.Repeat(" ", 2048)), "zstd"), http.StatusBadRequest},
		{"wire limit", "", []byte(strings.Repeat(" ", 2048)), http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := codexRequest(t, server, test.body, test.encoding)
			response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("status=%d want=%d", response.StatusCode, test.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached upstream %d times", calls.Load())
	}
}

func TestCodexJSONResponseAndUpstreamErrorsArePreserved(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := []byte(`{"model":"native-model","stream":false,"store":true,"temperature":0.5}`)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, body) {
					t.Error("nonstream request body changed")
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Upstream", "preserved")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, codexCompleted)
			}))
			defer upstream.Close()
			recorder := newCaptureRecorder()
			server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", enableCodex, recorder)
			defer cleanup()
			response := codexRequest(t, server, body, "")
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != status || string(got) != codexCompleted || response.Header.Get("X-Upstream") != "preserved" {
				t.Fatalf("response changed: %d %s %v", response.StatusCode, got, err)
			}
			completion := recorder.waitForCompletion(t)
			want := requestlog.OutcomeSuccess
			if status >= 400 {
				want = requestlog.OutcomeFailure
			}
			if completion.Outcome != want {
				t.Fatalf("outcome=%s want=%s", completion.Outcome, want)
			}
		})
	}
}

func TestCodexCatalogUsesProxyCredentialsAndDoesNotRecordInference(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.URL.RawQuery != "client_version=0.164.0&scope=a%2Fb" || r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("ChatGPT-Account-ID") != "test-account" {
			t.Error("unexpected native catalog request")
		}
		w.Header().Set("ETag", `"native"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"slug":"native-model"}],"future":true}`)
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", enableCodex, recorder)
	defer cleanup()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/codex/models?client_version=0.164.0&scope=a%2Fb", nil)
	request.Header.Set("Authorization", "Bearer client-chatgpt-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"native"` || string(got) != `{"models":[{"slug":"native-model"}],"future":true}` {
		t.Fatalf("catalog changed: status=%d body=%s error=%v", response.StatusCode, got, err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.requests) != 0 || len(recorder.completions) != 0 {
		t.Fatal("catalog counted as inference")
	}
}

// Exercises the real built-in OpenAI provider, ChatGPT auth, automatic catalog
// discovery, WebSocket-to-HTTP fallback, and zstd request compression locally.
func TestCodexBuiltinProviderWithRealClient(t *testing.T) {
	t.Run("responses", func(t *testing.T) { testCodexBuiltinProvider(t, false) })
	t.Run("standalone_search_and_images", func(t *testing.T) { testCodexBuiltinProvider(t, true) })
}

func testCodexBuiltinProvider(t *testing.T, withTools bool) {
	t.Helper()
	codexPath := os.Getenv("CODEX_PASS_TEST_CODEX_BIN")
	if codexPath == "" {
		t.Skip("set CODEX_PASS_TEST_CODEX_BIN to run the Codex client integration test")
	}
	codexHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, codexPath, args...)
		cmd.Dir = codexHome
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if name != "CODEX_HOME" && name != "CODEX_API_KEY" && name != "OPENAI_API_KEY" {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "CODEX_HOME="+codexHome)
		return cmd
	}
	bundled, err := command("debug", "models", "--bundled").Output()
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(bundled, &catalog); err != nil || len(catalog.Models) == 0 {
		t.Fatalf("bundled catalog: %v", err)
	}
	model := catalog.Models[0]
	model["slug"] = json.RawMessage(`"gpt-6.1-terra"`)
	model["visibility"] = json.RawMessage(`"list"`)
	if withTools {
		model["web_search_tool_type"] = json.RawMessage(`"text_and_image"`)
	}
	catalogBody, err := json.Marshal(map[string]any{"models": []any{model}})
	if err != nil {
		t.Fatal(err)
	}
	jwt := func(claims string) string {
		return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".fake"
	}
	claims := `{"exp":4102444800,"email":"test@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"client-account","chatgpt_plan_type":"plus"}}`
	authBody, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"id_token": jwt(claims), "access_token": jwt(claims), "refresh_token": "fake-refresh", "account_id": "client-account"}, "last_refresh": "2099-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), authBody, 0600); err != nil {
		t.Fatal(err)
	}
	referencePath := filepath.Join(codexHome, "reference.png")
	imageBytes, err := base64.StdEncoding.DecodeString(toolImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(referencePath, imageBytes, 0600); err != nil {
		t.Fatal(err)
	}
	var modelCalls, responseCalls, searchCalls, generationCalls, editCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("ChatGPT-Account-ID") != "test-account" {
			t.Error("client credentials leaked upstream")
		}
		if r.URL.Path == "/models" {
			modelCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalogBody)
			return
		}
		if withTools && r.URL.Path == "/alpha/search" {
			searchCalls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte(`"search_query"`)) || r.Header.Get("Accept") != "application/json" {
				t.Errorf("unexpected real-client search request: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, searchToolResponse)
			return
		}
		if withTools && (r.URL.Path == "/images/generations" || r.URL.Path == "/images/edits") {
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte(`"model":"gpt-image-2"`)) || r.Header.Get("Accept") != "application/json" {
				t.Errorf("unexpected real-client image request: %s", body)
			}
			if r.URL.Path == "/images/edits" {
				editCalls.Add(1)
				if !bytes.Contains(body, []byte(`"images"`)) || !bytes.Contains(body, []byte("data:image/png;base64,")) {
					t.Error("image edit lost the reference image")
				}
			} else {
				generationCalls.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Codex-Imagegen-Request-ID", "real-client-fixture")
			_, _ = io.WriteString(w, imageToolResponse)
			return
		}
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			t.Errorf("unexpected upstream %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		responseNumber := responseCalls.Add(1)
		if r.Header.Get("Content-Encoding") != "zstd" {
			t.Errorf("native request encoding=%q", r.Header.Get("Content-Encoding"))
		}
		wire, _ := io.ReadAll(r.Body)
		inspection := &Server{maxBodyBytes: defaultMaxRequestBody}
		body, err := inspection.decodeCodexBody(wire, r.Header.Get("Content-Encoding"))
		if err != nil {
			t.Error(err)
		}
		var payload struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Model != "gpt-6.1-terra" || !payload.Stream {
			t.Errorf("unexpected native payload model=%q stream=%t error=%v", payload.Model, payload.Stream, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if withTools && responseNumber == 1 {
			created, _ := json.Marshal(map[string]any{"type": "response.created", "response": map[string]string{"id": "resp_tools"}})
			_, _ = fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", created)
			for _, call := range []struct {
				id, namespace, name string
				arguments           any
			}{
				{"call_search", "web", "run", map[string]any{"search_query": []any{map[string]string{"q": "fixture"}}}},
				{"call_generate", "image_gen", "imagegen", map[string]any{"prompt": "fixture image", "transparent_background": false}},
				{"call_edit", "image_gen", "imagegen", map[string]any{"prompt": "fixture edit", "referenced_image_paths": []string{referencePath}}},
			} {
				arguments, _ := json.Marshal(call.arguments)
				event, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "call_id": call.id, "namespace": call.namespace, "name": call.name, "arguments": string(arguments)}})
				_, _ = fmt.Fprintf(w, "event: response.output_item.done\ndata: %s\n\n", event)
			}
			_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", strings.ReplaceAll(codexCompleted, "resp_codex", "resp_tools"))
			return
		}
		if withTools && (!bytes.Contains(body, []byte("search result")) || !bytes.Contains(body, []byte("data:image/png;base64,"))) {
			t.Error("real client did not return successful search and image tool outputs to the model")
		}
		_, _ = io.WriteString(w, codexSSE)
	}))
	defer upstream.Close()
	recorder := newCaptureRecorder()
	server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", enableCodex, recorder)
	defer cleanup()
	searchMode := "disabled"
	if withTools {
		searchMode = "live"
	}
	config := fmt.Sprintf("model_provider = \"openai\"\nmodel = \"gpt-6.1-terra\"\nopenai_base_url = %q\ncli_auth_credentials_store = \"file\"\nweb_search = %q\n[features]\napps = false\nplugins = false\ncode_mode = false\nuse_agent_identity = false\nenable_request_compression = true\napi_key_model_discovery = false\nstandalone_web_search = %t\nimage_generation = true\n", server.URL+"/codex", searchMode, withTools)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := command("exec", "--skip-git-repo-check", "--ephemeral", "--json", "Reply with OK.")
	output, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("Codex exec failed: %v\n%s\n%s", err, output, exit.Stderr)
		}
		t.Fatal(err)
	}
	wantResponseCalls := int32(1)
	if withTools {
		wantResponseCalls = 2
	}
	if modelCalls.Load() == 0 || responseCalls.Load() != wantResponseCalls || !bytes.Contains(output, []byte(`"type":"turn.completed"`)) {
		t.Fatalf("incomplete native flow: models=%d responses=%d events=%s", modelCalls.Load(), responseCalls.Load(), output)
	}
	if withTools {
		if searchCalls.Load() != 1 || generationCalls.Load() != 1 || editCalls.Load() != 1 {
			t.Fatalf("real client tool calls: search=%d generate=%d edit=%d events=%s", searchCalls.Load(), generationCalls.Load(), editCalls.Load(), output)
		}
		for range 5 {
			select {
			case <-recorder.finished:
			case <-time.After(2 * time.Second):
				t.Fatal("real client request accounting did not finish")
			}
		}
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		if len(recorder.completions) != 5 {
			t.Fatalf("real client flow recorded %d completions, want 5", len(recorder.completions))
		}
		for _, completion := range recorder.completions {
			if completion.Outcome != requestlog.OutcomeSuccess {
				t.Fatalf("real client completion=%+v", completion)
			}
		}
		return
	}
	if completion := recorder.waitForCompletion(t); completion.Outcome != requestlog.OutcomeSuccess {
		t.Fatalf("native accounting=%+v", completion)
	}
}
