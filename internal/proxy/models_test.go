package proxy

import (
	"bufio"
	"bytes"
	"context"
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
)

func TestModelsCatalogPreservesMetadataQueryAndHeaders(t *testing.T) {
	const query = "client_version=0.164.0&scope=a%2Fb&scope=c"
	const catalog = "{\n  \"models\": [{\"slug\":\"gpt-6.1-terra\",\"visibility\":\"list\",\"model_instructions\":{\"base_instructions\":\"keep these\"},\"context_window\":1048576,\"future_field\":{\"preserve\":true}}]\n}\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/models" || request.URL.RawQuery != query {
			t.Errorf("upstream request = %s %s", request.Method, request.URL)
		}
		for name, want := range map[string]string{
			"Authorization":       "Bearer test-access",
			"ChatGPT-Account-ID":  "test-account",
			"Accept":              "application/json",
			"Content-Type":        "",
			"Session-ID":          "catalog-session",
			"Originator":          "codex-cli",
			"If-None-Match":       `"previous"`,
			"X-Client-Metadata":   "preserved",
			"Proxy-Authorization": "",
		} {
			if got := request.Header.Get(name); got != want {
				t.Errorf("upstream %s = %q, want %q", name, got, want)
			}
		}
		body, _ := io.ReadAll(request.Body)
		if len(body) != 0 {
			t.Errorf("upstream GET has a body: %q", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", `"catalog-version"`)
		writer.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = io.WriteString(writer, catalog)
	}))
	defer upstream.Close()
	logPath := filepath.Join(t.TempDir(), "models.jsonl")
	recorder := newCaptureRecorder()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", logPath, recorder)
	defer cleanup()
	request, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/v1/models?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-local-secret")
	request.Header.Set("ChatGPT-Account-ID", "client-account-must-be-replaced")
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("session-id", "catalog-session")
	request.Header.Set("originator", "codex-cli")
	request.Header.Set("If-None-Match", `"previous"`)
	request.Header.Set("X-Client-Metadata", "preserved")
	request.Header.Set("Proxy-Authorization", "Bearer proxy-secret")
	response, err := proxyServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != catalog {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if response.Header.Get("ETag") != `"catalog-version"` || response.Header.Get("Cache-Control") != "private, max-age=300" || response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("catalog response headers = %v", response.Header)
	}
	recorder.mu.Lock()
	requests, completions := len(recorder.requests), len(recorder.completions)
	recorder.mu.Unlock()
	if requests != 0 || completions != 0 {
		t.Fatalf("catalog recorded as inference usage: requests=%d completions=%d", requests, completions)
	}
	cleanup()
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logs, []byte(`"mode":"models"`)) || !bytes.Contains(logs, []byte(`"method":"GET"`)) {
		t.Fatalf("missing catalog debug records: %s", logs)
	}
	for _, secret := range []string{"test-access", "sk-local-secret", "proxy-secret"} {
		if bytes.Contains(logs, []byte(secret)) {
			t.Fatalf("catalog debug log contains credential %q", secret)
		}
	}
}

func TestModelsRequiresLocalAPIKeyAndGET(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()
	for _, test := range []struct {
		name, method, authorization string
		status                      int
	}{
		{"missing key", http.MethodGet, "", http.StatusUnauthorized},
		{"incorrect key", http.MethodGet, "Bearer incorrect", http.StatusUnauthorized},
		{"POST", http.MethodPost, "Bearer sk-local-secret", http.StatusMethodNotAllowed},
		{"HEAD", http.MethodHead, "Bearer sk-local-secret", http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, proxyServer.URL+"/v1/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", test.authorization)
			response, err := proxyServer.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			if test.status == http.StatusMethodNotAllowed && response.Header.Get("Allow") != http.MethodGet {
				t.Fatalf("Allow = %q", response.Header.Get("Allow"))
			}
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("unauthorized or unsupported requests reached upstream %d times", upstreamCalls.Load())
	}
}

func TestModelsUpstreamErrorsAndNotModifiedAreTransparent(t *testing.T) {
	for _, status := range []int{http.StatusNotModified, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			payload := `{"error":{"message":"catalog unavailable"}}`
			if status == http.StatusNotModified {
				payload = ""
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("ETag", `"catalog"`)
				writer.Header().Set("Retry-After", "10")
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, payload)
			}))
			defer upstream.Close()
			proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
			defer cleanup()
			response := getModels(t, proxyServer)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != status || string(body) != payload || response.Header.Get("ETag") != `"catalog"` || response.Header.Get("Retry-After") != "10" {
				t.Fatalf("status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
			}
		})
	}
}

func TestModelsRefreshesOnceAndRetriesTheSameGET(t *testing.T) {
	for _, test := range []struct {
		name          string
		refreshStatus int
		secondStatus  int
		wantCalls     int32
		wantStatus    int
		wantBody      string
	}{
		{"success", http.StatusOK, http.StatusOK, 2, http.StatusOK, `{"models":[{"slug":"gpt-6.1-terra"}]}`},
		{"still unauthorized", http.StatusOK, http.StatusUnauthorized, 2, http.StatusUnauthorized, `{"error":"unauthorized"}`},
		{"refresh rejected", http.StatusBadRequest, 0, 1, http.StatusUnauthorized, `{"error":"unauthorized"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls, refreshCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/refresh":
					refreshCalls.Add(1)
					if request.Method != http.MethodPost {
						t.Errorf("refresh method = %s", request.Method)
					}
					writer.WriteHeader(test.refreshStatus)
					_, _ = io.WriteString(writer, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
				case "/models":
					call := calls.Add(1)
					if request.Method != http.MethodGet || request.URL.RawQuery != "client_version=0.164.0" || request.Header.Get("Accept") != "application/json" {
						t.Errorf("catalog retry changed: %s %s headers=%v", request.Method, request.URL, request.Header)
					}
					wantToken := "Bearer test-access"
					if call > 1 {
						wantToken = "Bearer rotated-access"
					}
					if request.Header.Get("Authorization") != wantToken || request.Header.Get("ChatGPT-Account-ID") != "test-account" {
						t.Errorf("incorrect catalog credentials on attempt %d", call)
					}
					status := http.StatusUnauthorized
					if call > 1 {
						status = test.secondStatus
					}
					writer.WriteHeader(status)
					if status == http.StatusOK {
						_, _ = io.WriteString(writer, test.wantBody)
					} else {
						_, _ = io.WriteString(writer, `{"error":"unauthorized"}`)
					}
				default:
					t.Errorf("unexpected upstream path %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
			defer cleanup()
			response := getModels(t, proxyServer)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != test.wantStatus || string(body) != test.wantBody {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
			if calls.Load() != test.wantCalls || refreshCalls.Load() != 1 {
				t.Fatalf("catalog calls=%d refresh calls=%d", calls.Load(), refreshCalls.Load())
			}
		})
	}
}

func TestModelsUpstreamConnectionFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()
	upstream.Close()
	response := getModels(t, proxyServer)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "upstream request failed") {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}

// Set CODEX_PASS_TEST_CODEX_BIN to exercise the installed Codex catalog reader
// against this proxy and a local upstream, without using real credentials.
func TestModelsCatalogWithCodexClient(t *testing.T) {
	codexPath := os.Getenv("CODEX_PASS_TEST_CODEX_BIN")
	if codexPath == "" {
		t.Skip("set CODEX_PASS_TEST_CODEX_BIN to run the Codex client integration test")
	}
	codexHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runCodex := func(arguments ...string) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, codexPath, arguments...)
		command.Dir = codexHome
		command.Env = append(os.Environ(), "CODEX_HOME="+codexHome, "CODEX_PASS_TEST_API_KEY=sk-local-secret")
		output, err := command.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				t.Fatalf("Codex failed: %v\n%s", err, exitErr.Stderr)
			}
			t.Fatal(err)
		}
		return output
	}
	var bundled struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(runCodex("debug", "models", "--bundled"), &bundled); err != nil || len(bundled.Models) == 0 {
		t.Fatalf("read bundled Codex catalog: %v", err)
	}
	model := bundled.Models[0]
	model["slug"] = json.RawMessage(`"gpt-6.1-terra"`)
	model["display_name"] = json.RawMessage(`"GPT-6.1 Terra"`)
	model["visibility"] = json.RawMessage(`"list"`)
	catalog, err := json.Marshal(map[string]any{"models": []any{model}})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/models" || request.URL.Query().Get("client_version") == "" || request.Header.Get("Authorization") != "Bearer test-access" {
			t.Errorf("unexpected Codex catalog upstream request: %s %s", request.Method, request.URL)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(catalog)
	}))
	defer upstream.Close()
	proxyServer, cleanup := newTestProxy(t, upstream, upstream.URL+"/refresh", "")
	defer cleanup()
	config := fmt.Sprintf(`model_provider = "codex_pass"
[features]
api_key_model_discovery = true
[model_providers.codex_pass]
name = "Codex Pass"
base_url = %q
wire_api = "responses"
env_key = "CODEX_PASS_TEST_API_KEY"
model_catalog_url = %q
`, proxyServer.URL+"/v1", proxyServer.URL+"/v1/models")
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var actual struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(runCodex("debug", "models"), &actual); err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 || len(actual.Models) != 1 || actual.Models[0].Slug != "gpt-6.1-terra" {
		t.Fatalf("Codex did not load the proxy catalog: upstream calls=%d models=%v", calls.Load(), actual.Models)
	}
	t.Run("desktop_runtime_discovery_default", func(t *testing.T) {
		// The desktop can disable discovery through runtime defaults. An explicit
		// config value must keep the proxy catalog available to its model picker.
		command := exec.CommandContext(ctx, codexPath, "app-server", "--stdio")
		command.Dir = codexHome
		command.Env = append(os.Environ(), "CODEX_HOME="+codexHome, "CODEX_PASS_TEST_API_KEY=sk-local-secret")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		stdin, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = stdin.Close()
			_ = command.Process.Kill()
			_ = command.Wait()
			if t.Failed() {
				t.Logf("app-server stderr: %s", stderr.String())
			}
		}()
		encoder := json.NewEncoder(stdin)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 65536), 2<<20)
		rpc := func(id int, method string, params any) json.RawMessage {
			t.Helper()
			if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
				t.Fatal(err)
			}
			for scanner.Scan() {
				var response struct {
					ID     *int            `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.ID == nil || *response.ID != id {
					continue
				}
				if len(response.Error) > 0 {
					t.Fatalf("%s failed: %s", method, response.Error)
				}
				return response.Result
			}
			t.Fatalf("app-server stopped before %s response: %v", method, scanner.Err())
			return nil
		}
		rpc(0, "initialize", map[string]any{
			"clientInfo":   map[string]string{"name": "codex-pass-test", "version": "1.0.0"},
			"capabilities": map[string]bool{"experimentalApi": true},
		})
		if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
			t.Fatal(err)
		}
		rpc(1, "experimentalFeature/enablement/set", map[string]any{
			"enablement": map[string]bool{"api_key_model_discovery": false},
		})
		var picker struct {
			Data []struct {
				Model  string `json:"model"`
				Hidden bool   `json:"hidden"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rpc(2, "model/list", map[string]any{"includeHidden": false}), &picker); err != nil {
			t.Fatal(err)
		}
		if len(picker.Data) != 1 || picker.Data[0].Model != "gpt-6.1-terra" || picker.Data[0].Hidden {
			t.Fatalf("desktop did not expose the proxy catalog: %+v", picker.Data)
		}
	})
}

func getModels(t *testing.T, proxyServer *httptest.Server) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/v1/models?client_version=0.164.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer sk-local-secret")
	response, err := proxyServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
