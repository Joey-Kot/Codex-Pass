package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"codex-pass/internal/requestlog"
)

const toolImage = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
const searchToolResponse = `{"encrypted_output":"opaque","output":"search result","results":[{"type":"text_result","ref_id":"turn0search0","url":"https://example.com","future":true}]}`
const imageToolResponse = `{"created":1,"background":"opaque","data":[{"b64_json":"` + toolImage + `","generation_id":"gen-test"}],"usage":{"input_tokens":17,"input_tokens_details":{"image_tokens":10,"text_tokens":7},"output_tokens":20,"output_tokens_details":{"image_tokens":20},"total_tokens":37}}`

func TestCodexToolsPreserveJSONCompressionHeadersAndUsage(t *testing.T) {
	for _, path := range []string{"alpha/search", "images/generations", "images/edits"} {
		for _, prefix := range []string{"/v1/", "/codex/"} {
			for _, encoding := range []string{"", "zstd"} {
				t.Run(prefix+path+"/"+encoding, func(t *testing.T) {
					model := "gpt-image-2"
					result := imageToolResponse
					if path == "alpha/search" {
						model = "gpt-6.1-sol"
						result = searchToolResponse
					}
					body := []byte("{\n\"model\":\"" + model + "\",\"reasoning\":{\"effort\":\"high\"},\"prompt\":\"fixture\",\"commands\":{\"search_query\":[{\"q\":\"fixture\"}]},\"images\":[{\"image_url\":\"data:image/png;base64," + toolImage + "\"}],\"prompt_cache_key\":\"tool-cache\",\"store\":true,\"temperature\":0.75,\"future\":9007199254740993\n}\n")
					wire := encodeCodexTestBody(t, body, encoding)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						got, _ := io.ReadAll(r.Body)
						if r.Method != http.MethodPost || r.URL.Path != "/"+path || r.URL.RawQuery != "scope=a%2Fb&scope=c" || !bytes.Equal(got, wire) {
							t.Errorf("tool request changed: %s %s bodyEqual=%t", r.Method, r.URL, bytes.Equal(got, wire))
						}
						for key, want := range map[string]string{"Accept": "application/json", "Accept-Encoding": "identity", "Content-Type": "application/json", "Content-Encoding": encoding, "Authorization": "Bearer test-access", "ChatGPT-Account-ID": "test-account", "Originator": "codex_cli_rs", "X-Codex-Turn-Metadata": "test-metadata", "X-Codex-Image-Turn-Id": "test-turn", "Session-ID": "tool-session"} {
							if got := r.Header.Get(key); got != want {
								t.Errorf("%s=%q want=%q", key, got, want)
							}
						}
						w.Header().Set("Content-Type", "application/json; charset=utf-8")
						w.Header().Set("X-Codex-Imagegen-Request-ID", "req-image-test")
						_, _ = io.WriteString(w, result)
					}))
					defer upstream.Close()
					recorder := newCaptureRecorder()
					logPath := filepath.Join(t.TempDir(), "tools.jsonl")
					server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", logPath, enableCodex, recorder)
					defer cleanup()
					request, _ := http.NewRequest(http.MethodPost, server.URL+prefix+path+"?scope=a%2Fb&scope=c", bytes.NewReader(wire))
					request.Header.Set("Content-Type", "application/json; charset=utf-8")
					request.Header.Set("Content-Encoding", encoding)
					request.Header.Set("Authorization", "Bearer client-chatgpt-token")
					if prefix == "/v1/" {
						request.Header.Set("Authorization", "Bearer sk-local-secret")
					}
					request.Header.Set("ChatGPT-Account-ID", "client-account")
					request.Header.Set("Originator", "codex_cli_rs")
					request.Header.Set("X-Codex-Turn-Metadata", "test-metadata")
					request.Header.Set("X-Codex-Image-Turn-Id", "test-turn")
					request.Header.Set("session-id", "tool-session")
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || response.StatusCode != http.StatusOK || string(got) != result || response.Header.Get("X-Codex-Imagegen-Request-ID") != "req-image-test" {
						t.Fatalf("tool response changed: %d %s error=%v", response.StatusCode, got, err)
					}
					completion := recorder.waitForCompletion(t)
					recorded := recorder.waitForRequestStart(t)
					endpoint := path
					if prefix == "/codex/" {
						endpoint = "codex/" + path
					}
					if recorded.Endpoint != endpoint || recorded.Model != model || recorded.ReasoningEffort != "high" || completion.Model != model || completion.Outcome != requestlog.OutcomeSuccess {
						t.Fatalf("accounting: request=%+v completion=%+v", recorded, completion)
					}
					if path == "alpha/search" {
						if completion.InputTokens != nil || completion.OutputTokens != nil {
							t.Fatal("missing search usage was fabricated")
						}
					} else if completion.InputTokens == nil || *completion.InputTokens != 17 || completion.OutputTokens == nil || *completion.OutputTokens != 20 || completion.CacheReadTokens != nil || completion.ReasoningTokens != nil {
						t.Fatalf("image usage=%+v", completion)
					}
					cleanup()
					logs, err := os.ReadFile(logPath)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Contains(logs, []byte(`"changed_fields":[],"removed_fields":[],"added_fields":[]`)) || !bytes.Contains(logs, []byte(`"stage":"UPSTREAM_COMPLETED"`)) {
						t.Fatal("missing tool debug records")
					}
					for _, secret := range []string{"client-chatgpt-token", "sk-local-secret", "test-access"} {
						if bytes.Contains(logs, []byte(secret)) {
							t.Fatalf("debug log contains %q", secret)
						}
					}
				})
			}
		}
	}
}

func TestCodexToolRoutesHonorAuthenticationAndOptIn(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, searchToolResponse)
	}))
	defer upstream.Close()
	for _, enabled := range []bool{false, true} {
		server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", func(c *Config) { c.EnableCodex = enabled })
		for _, path := range []string{"alpha/search", "images/generations", "images/edits"} {
			for _, prefix := range []string{"/v1/", "/codex/"} {
				for _, token := range []string{"", "client-chatgpt-token", "sk-local-secret"} {
					request, _ := http.NewRequest(http.MethodPost, server.URL+prefix+path, strings.NewReader(`{"model":"test"}`))
					if token != "" {
						request.Header.Set("Authorization", "Bearer "+token)
					}
					want := http.StatusOK
					if prefix == "/codex/" && !enabled {
						want = http.StatusNotFound
					} else if prefix == "/v1/" && token != "sk-local-secret" {
						want = http.StatusUnauthorized
					}
					before := calls.Load()
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode != want {
						t.Fatalf("enabled=%t route=%s token=%t status=%d want=%d", enabled, prefix+path, token != "", response.StatusCode, want)
					}
					if want != http.StatusOK && calls.Load() != before {
						t.Fatal("rejected request reached upstream")
					}
				}
				if prefix == "/v1/" || enabled {
					response, err := server.Client().Get(server.URL + prefix + path)
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodPost {
						t.Fatal("incorrect tool method handling")
					}
				}
			}
		}
		response, err := server.Client().Post(server.URL+"/codex/alpha/../responses", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatal("non-allowlisted tool path was accepted")
		}
		cleanup()
	}
}

func TestCodexToolsRefreshOnceAndPreserveUpstreamErrors(t *testing.T) {
	for _, path := range []string{"alpha/search", "images/generations", "images/edits"} {
		for _, lastStatus := range []int{http.StatusOK, http.StatusUnauthorized} {
			t.Run(path+"/"+http.StatusText(lastStatus), func(t *testing.T) {
				var calls, refreshes atomic.Int32
				wire := encodeCodexTestBody(t, []byte(`{"model":"test","prompt":"test"}`), "zstd")
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/refresh" {
						refreshes.Add(1)
						_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
						return
					}
					got, _ := io.ReadAll(r.Body)
					if !bytes.Equal(got, wire) || r.Header.Get("Content-Encoding") != "zstd" {
						t.Error("tool retry changed request bytes")
					}
					if calls.Add(1) == 1 {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, `{"error":"expired"}`)
						return
					}
					if r.Header.Get("Authorization") != "Bearer new-access" {
						t.Error("tool retry did not use refreshed token")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(lastStatus)
					_, _ = io.WriteString(w, `{"output":"exact response"}`)
				}))
				defer upstream.Close()
				recorder := newCaptureRecorder()
				server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", enableCodex, recorder)
				defer cleanup()
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/codex/"+path, bytes.NewReader(wire))
				request.Header.Set("Content-Encoding", "zstd")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != lastStatus || string(got) != `{"output":"exact response"}` || calls.Load() != 2 || refreshes.Load() != 1 {
					t.Fatalf("retry: status=%d calls=%d refreshes=%d body=%s error=%v", response.StatusCode, calls.Load(), refreshes.Load(), got, err)
				}
				completion := recorder.waitForCompletion(t)
				want := requestlog.OutcomeSuccess
				if lastStatus != http.StatusOK {
					want = requestlog.OutcomeFailure
				}
				if completion.Outcome != want {
					t.Fatalf("tool outcome=%s want=%s", completion.Outcome, want)
				}
			})
		}
	}
}

func TestCodexToolsRejectUnsupportedBodies(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	server, cleanup := newTestProxyConfigured(t, upstream, upstream.URL+"/refresh", "", func(c *Config) { c.EnableCodex = true; c.MaxBodyBytes = 1024 })
	defer cleanup()
	for _, test := range []struct {
		body, contentType, encoding string
		want                        int
	}{
		{"[]", "application/json", "", http.StatusBadRequest},
		{"broken", "application/json", "", http.StatusBadRequest},
		{"{}", "multipart/form-data; boundary=test", "", http.StatusUnsupportedMediaType},
		{"broken", "application/json", "zstd", http.StatusBadRequest},
		{"{}", "application/json", "br", http.StatusUnsupportedMediaType},
		{strings.Repeat(" ", 2048), "application/json", "", http.StatusRequestEntityTooLarge},
	} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/codex/images/edits", strings.NewReader(test.body))
		request.Header.Set("Content-Type", test.contentType)
		request.Header.Set("Content-Encoding", test.encoding)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Fatalf("invalid body status=%d want=%d", response.StatusCode, test.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid tool body reached upstream")
	}
}
