package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"codex-pass/internal/auth"
	"codex-pass/internal/proxy"

	"github.com/google/uuid"
)

const resetCreditFixture = `{
  "available_count": 2,
  "credits": [
    {"id":"second", "status":"available", "title":"Second reset", "granted_at":"2026-07-02T12:00:00Z", "expires_at":null},
    {"id":"used", "status":"redeemed", "title":"Used reset", "granted_at":"2026-07-01T12:00:00Z"},
    {"id":"first", "status":"available", "title":"First reset", "granted_at":"2026-07-01T12:00:00Z", "expires_at":"2026-08-01T12:00:00Z"}
  ]
}`

func TestQuotaShowsWeeklyWindows(t *testing.T) {
	code, output, diagnostics := runMockAccountCommand(t, []string{"quota"}, "", func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		return accountResponse(http.StatusOK, `{
          "plan_type":"plus",
          "rate_limit":{
            "primary_window":{"limit_window_seconds":18000,"used_percent":99},
            "secondary_window":{"limit_window_seconds":604800,"used_percent":12.5,"reset_at":1900000000}
          },
          "additional_rate_limits":[{
            "limit_name":"Extra",
            "rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":30}}
          }]
        }`)
	})
	if code != 0 || diagnostics != "" || !strings.Contains(output, "Weekly quota (plus)") ||
		!strings.Contains(output, "12.5%") || !strings.Contains(output, "87.5%") ||
		!strings.Contains(output, "Extra") || !strings.Contains(output, "70%") || strings.Contains(output, "99%") {
		t.Fatalf("quota exit=%d\n%s\n%s", code, output, diagnostics)
	}
}

func TestAccountRefreshDoesNotStrandProxy(t *testing.T) {
	for _, test := range []struct{ name, lastRefresh string }{
		{name: "proactive", lastRefresh: "2000-01-01T00:00:00Z"},
		{name: "after unauthorized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shared-auth.json")
			contents := fmt.Sprintf(`{"auth_mode":"chatgpt","last_refresh":%q,"tokens":{"access_token":"old-access","refresh_token":"old-refresh","account_id":"account"}}`, test.lastRefresh)
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			refreshes := 0
			upstream := mockAccountTransport(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/oauth/token":
					refreshes++
					if refreshes != 1 {
						return accountResponse(http.StatusBadRequest, `{"error":"invalid_grant"}`)
					}
					return accountResponse(http.StatusOK, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
				case "/backend-api/wham/usage", "/backend-api/codex/models":
					if request.Header.Get("Authorization") != "Bearer new-access" {
						return accountResponse(http.StatusUnauthorized, "{}")
					}
					return accountResponse(http.StatusOK, `{"plan_type":"plus","models":[]}`)
				default:
					t.Fatalf("unexpected upstream request %s %s", request.Method, request.URL)
					return nil, nil
				}
			})
			credentials, err := auth.LoadAll([]string{path})
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: upstream}
			pool, err := auth.NewPool(credentials, auth.RefreshConfig{Client: client})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := proxy.New(proxy.Config{APIKeys: []string{"proxy-key"}, AuthPool: pool, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}

			t.Setenv("CODEXPASS_AUTH_FILE", path)
			if code, _, diagnostics := runMockAccountCLI(t, []string{"quota"}, "", upstream); code != 0 {
				t.Fatalf("quota exit=%d: %s", code, diagnostics)
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer proxy-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || refreshes != 1 || credentials[0].Snapshot().Token != "new-access" {
				t.Fatalf("proxy response=%d, OAuth refreshes=%d: %s", response.Code, refreshes, response.Body.String())
			}
		})
	}
}

func TestResetListsCreditsAndDefaultsToNo(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		input  string
		cancel bool
	}{
		{name: "list", args: []string{"reset"}},
		{name: "enter", args: []string{"reset", "2"}, input: "\n", cancel: true},
		{name: "explicit no", args: []string{"reset", "2"}, input: "N\n", cancel: true},
		{name: "invalid input", args: []string{"reset", "2"}, input: "yes\ny\n", cancel: true},
		{name: "closed input", args: []string{"reset", "2"}, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, output, diagnostics := runMockAccountCommand(t, test.args, test.input, func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.Path != "/backend-api/wham/rate-limit-reset-credits" {
					t.Fatalf("unexpected request %s %s", request.Method, request.URL)
				}
				return accountResponse(http.StatusOK, resetCreditFixture)
			})
			if code != 0 || diagnostics != "" {
				t.Fatalf("reset exit=%d\n%s\n%s", code, output, diagnostics)
			}
			if test.cancel {
				if !strings.Contains(output, "Reset 2 (Second reset)") || !strings.Contains(output, "[y/N]") || !strings.Contains(output, "Reset canceled.") {
					t.Fatalf("confirmation output = %q", output)
				}
			} else if !strings.Contains(output, "2 available") || !strings.Contains(output, "AUTH_FILE") ||
				!strings.Contains(output, "Expires at") || strings.Count(output, "auth.json") != 2 || strings.Contains(output, "| 3 |") {
				t.Fatalf("listing output = %q", output)
			}
		})
	}
}

func TestConfirmedResetUsesOneCreditAndReusesRequestID(t *testing.T) {
	first := writeMockAuthFile(t, "first.json", "first-account")
	second := writeMockAuthFile(t, "second.json", "second-account")
	t.Setenv("CODEXPASS_AUTH_FILE", strings.Join([]string{first, second}, string(os.PathListSeparator)))
	var calls []string
	var redeemBody string
	code, output, diagnostics := runMockAccountCLI(t, []string{"reset", "4"}, " Y \r\n", func(request *http.Request) (*http.Response, error) {
		calls = append(calls, strings.TrimSpace(request.Method+" "+request.URL.Path+" "+request.Header.Get("ChatGPT-Account-ID")))
		switch request.URL.Path {
		case "/backend-api/wham/rate-limit-reset-credits":
			return accountResponse(http.StatusOK, resetCreditFixture)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]string
			if err := json.Unmarshal(body, &payload); err != nil || payload["credit_id"] != "second" {
				t.Fatalf("incorrect redemption payload %q: %v", body, err)
			}
			if _, err := uuid.Parse(payload["redeem_request_id"]); err != nil {
				t.Fatalf("invalid redeem_request_id: %v", err)
			}
			if redeemBody == "" {
				if request.Header.Get("Authorization") != "Bearer second-account-access" {
					t.Fatal("redemption did not use the selected file's access token")
				}
				redeemBody = string(body)
				return accountResponse(http.StatusUnauthorized, "{}")
			}
			if string(body) != redeemBody || request.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Fatal("retry changed the selected credit or idempotency key, or did not use the refreshed token")
			}
			return accountResponse(http.StatusOK, `{"code":"reset","windows_reset":2}`)
		case "/oauth/token":
			return accountResponse(http.StatusOK, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
		case "/backend-api/wham/usage":
			return accountResponse(http.StatusOK, `{"plan_type":"plus","rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":0}}}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
			return nil, nil
		}
	})
	wantCalls := []string{
		"GET /backend-api/wham/rate-limit-reset-credits first-account",
		"GET /backend-api/wham/rate-limit-reset-credits second-account",
		"POST /backend-api/wham/rate-limit-reset-credits/consume second-account",
		"POST /oauth/token",
		"POST /backend-api/wham/rate-limit-reset-credits/consume second-account",
		"GET /backend-api/wham/usage second-account",
	}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("request sequence = %v, want %v", calls, wantCalls)
	}
	if code != 0 || diagnostics != "" || !strings.Contains(output, "reset 4 redeemed") ||
		!strings.Contains(output, "AUTH_FILE second.json") || !strings.Contains(output, "100%") {
		t.Fatalf("reset exit=%d\n%s\n%s", code, output, diagnostics)
	}
}

func TestResetListsOneSequenceAcrossFiles(t *testing.T) {
	first := writeMockAuthFile(t, "first.json", "first-account")
	second := writeMockAuthFile(t, "second.json", "second-account")
	code, output, diagnostics := runMockAccountCLI(t, []string{"reset", first, first, second}, "", func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/backend-api/wham/rate-limit-reset-credits" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		return accountResponse(http.StatusOK, resetCreditFixture)
	})
	if code != 0 || diagnostics != "" || !strings.Contains(output, "4 available") || strings.Count(output, "AUTH_FILE") != 1 {
		t.Fatalf("reset exit=%d\n%s\n%s", code, output, diagnostics)
	}
	for index, file := range []string{"first.json", "first.json", "second.json", "second.json"} {
		_, afterNumber, found := strings.Cut(output, fmt.Sprintf("| %d |", index+1))
		row, _, _ := strings.Cut(afterNumber, "\n")
		fileCell, expiryCell, _ := strings.Cut(row, "|")
		wantExpiry := "-"
		if index%2 == 0 {
			wantExpiry = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04:05 -07:00")
		}
		if !found || strings.TrimSpace(fileCell) != file || strings.TrimSpace(strings.TrimSuffix(expiryCell, "|")) != wantExpiry {
			t.Fatalf("reset row %d = %q, want file %s and expiry %s", index+1, row, file, wantExpiry)
		}
	}
}

func TestResetDoesNotNumberOrRedeemIncompleteList(t *testing.T) {
	first := writeMockAuthFile(t, "first.json", "first-account")
	second := writeMockAuthFile(t, "second.json", "second-account")
	upstream := mockAccountTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/backend-api/wham/rate-limit-reset-credits" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		if request.Header.Get("ChatGPT-Account-ID") == "first-account" {
			return accountResponse(http.StatusOK, `{"available_count":1,"credits":null}`)
		}
		return accountResponse(http.StatusOK, `{"available_count":1,"credits":[{"id":"second","status":"available"}]}`)
	})
	code, output, diagnostics := runMockAccountCLI(t, []string{"reset", first, second}, "", upstream)
	if code != 0 || diagnostics != "" || !strings.Contains(output, "2 available") ||
		!strings.Contains(output, "| - | second.json") || !strings.Contains(output, "complete credit details for first.json") {
		t.Fatalf("reset list exit=%d\n%s\n%s", code, output, diagnostics)
	}
	code, output, diagnostics = runMockAccountCLI(t, []string{"reset", "1", first, second}, "y\n", upstream)
	if code != 1 || output != "" || !strings.Contains(diagnostics, "details are incomplete for first.json") {
		t.Fatalf("reset redemption exit=%d\n%s\n%s", code, output, diagnostics)
	}
}

func TestResetStopsWhenAnAccountCannotBeListed(t *testing.T) {
	first := writeMockAuthFile(t, "first.json", "first-account")
	second := writeMockAuthFile(t, "second.json", "second-account")
	code, output, diagnostics := runMockAccountCLI(t, []string{"reset", "1", first, second}, "y\n", func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/backend-api/wham/rate-limit-reset-credits" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		if request.Header.Get("ChatGPT-Account-ID") == "second-account" {
			return accountResponse(http.StatusServiceUnavailable, "{}")
		}
		return accountResponse(http.StatusOK, resetCreditFixture)
	})
	if code != 1 || output != "" || !strings.Contains(diagnostics, "second.json") {
		t.Fatalf("reset exit=%d\n%s\n%s", code, output, diagnostics)
	}
}

func TestResetUncertainResultDoesNotRetry(t *testing.T) {
	posts := 0
	code, _, diagnostics := runMockAccountCommand(t, []string{"reset", "1"}, "y\n", func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet && request.URL.Path == "/backend-api/wham/rate-limit-reset-credits" {
			return accountResponse(http.StatusOK, resetCreditFixture)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
		}
		posts++
		return accountResponse(http.StatusServiceUnavailable, "{}")
	})
	if code != 1 || posts != 1 || !strings.Contains(diagnostics, "could not verify the redemption result") {
		t.Fatalf("reset exit=%d, attempts=%d, diagnostics=%q", code, posts, diagnostics)
	}
}

func TestModelsSeparatesAccountCatalogsAndImageTool(t *testing.T) {
	code, output, diagnostics := runMockAccountCommand(t, []string{"models", "--client-version", "0.166.1"}, "", func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected request method %s", request.Method)
		}
		switch request.URL.Path {
		case "/backend-api/codex/models":
			if version := request.URL.Query().Get("client_version"); version != "0.166.1" {
				t.Fatalf("unexpected client version %q", version)
			}
			return accountResponse(http.StatusOK, `{"models":[{"slug":"codex-hidden","display_name":"Hidden model","visibility":"hide"}]}`)
		case "/backend-api/models":
			return accountResponse(http.StatusOK, `{"models":[{"slug":"chatgpt-model","title":"Chat model","enabled_tools":["image_gen_tool_enabled"]}]}`)
		default:
			t.Fatalf("unexpected model catalog request %s", request.URL)
			return nil, nil
		}
	})
	if code != 0 || diagnostics != "" {
		t.Fatalf("models exit=%d\n%s\n%s", code, output, diagnostics)
	}
	for _, want := range []string{"Codex models", "codex-hidden", "hide", "ChatGPT models", "chatgpt-model", "yes", "Codex image tool", "gpt-image-2", "account access unverified"} {
		if !strings.Contains(output, want) {
			t.Errorf("models output missing %q:\n%s", want, output)
		}
	}
}

func TestQuotaAndModelsGroupAuthFiles(t *testing.T) {
	first := writeMockAuthFile(t, "auth.json", "first-account")
	second := writeMockAuthFile(t, "auth.json", "second-account")
	t.Setenv("CODEXPASS_AUTH_FILE", strings.Join([]string{first, second}, string(os.PathListSeparator)))
	for _, command := range []string{"quota", "models"} {
		t.Run(command, func(t *testing.T) {
			code, output, diagnostics := runMockAccountCLI(t, []string{command}, "", func(request *http.Request) (*http.Response, error) {
				account := request.Header.Get("ChatGPT-Account-ID")
				if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer "+account+"-access" {
					t.Fatalf("unexpected request for %s: %s %s", account, request.Method, request.URL)
				}
				switch request.URL.Path {
				case "/backend-api/wham/usage":
					return accountResponse(http.StatusOK, fmt.Sprintf(`{"plan_type":%q}`, account))
				case "/backend-api/codex/models", "/backend-api/models":
					return accountResponse(http.StatusOK, fmt.Sprintf(`{"models":[{"slug":%q}]}`, account))
				default:
					t.Fatalf("unexpected request %s", request.URL)
					return nil, nil
				}
			})
			before, secondSection, found := strings.Cut(output, "AUTH_FILE "+second+"\n")
			if code != 0 || diagnostics != "" || !found || !strings.HasPrefix(before, "AUTH_FILE "+first+"\n") ||
				!strings.Contains(before, "first-account") || strings.Contains(before, "second-account") ||
				!strings.Contains(secondSection, "second-account") || strings.Contains(secondSection, "first-account") {
				t.Fatalf("%s exit=%d\n%s\n%s", command, code, output, diagnostics)
			}
		})
	}
}

func TestAuthFileEnvironmentAndPositionalPrecedence(t *testing.T) {
	environment := []string{"first auth.json", "second.json"}
	t.Setenv("CODEXPASS_AUTH_FILE", strings.Join(environment, string(os.PathListSeparator)))
	explicit := []string{"other.json", "fourth.json"}
	for _, test := range []struct {
		arguments []string
		paths     []string
		number    int
	}{
		{arguments: []string{"--key", "test-key"}, paths: environment},
		{arguments: []string{"quota"}, paths: environment},
		{arguments: []string{"models", "--client-version", "0.166.1"}, paths: environment},
		{arguments: []string{"reset"}, paths: environment},
		{arguments: []string{"reset", "2"}, paths: environment, number: 2},
		{arguments: append([]string{"quota"}, explicit...), paths: explicit},
		{arguments: append([]string{"reset"}, explicit...), paths: explicit},
		{arguments: append([]string{"reset", "3"}, explicit...), paths: explicit, number: 3},
		{arguments: []string{"reset", "./2"}, paths: []string{"./2"}},
	} {
		options, err := parseCommandLine(test.arguments, io.Discard)
		if err != nil || !slices.Equal(options.authFiles, test.paths) || options.account.resetNumber != test.number {
			t.Errorf("parseCommandLine(%q) = files %q, reset number %d, error %v", test.arguments, options.authFiles, options.account.resetNumber, err)
		}
	}
}

func TestAccountCommandRejectsInvalidArguments(t *testing.T) {
	t.Setenv("CODEXPASS_AUTH_FILE", "")
	for _, arguments := range [][]string{
		{"quota"}, {"models"}, {"reset"}, {"reset", "1"},
		{"reset", "0", "auth.json"}, {"reset", "--", "-1", "auth.json"}, {"reset", "99999999999999999999999", "auth.json"},
	} {
		if _, err := parseCommandLine(arguments, io.Discard); err == nil {
			t.Errorf("parseCommandLine(%q) accepted invalid arguments", arguments)
		}
	}
}

type mockAccountTransport func(*http.Request) (*http.Response, error)

func (f mockAccountTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func accountResponse(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func runMockAccountCommand(t *testing.T, arguments []string, input string, respond mockAccountTransport) (int, string, string) {
	t.Helper()
	path := writeMockAuthFile(t, "auth.json", "test-account")
	return runMockAccountCLI(t, append(arguments, path), input, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "chatgpt.com":
			if request.Header.Get("ChatGPT-Account-ID") != "test-account" || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
				t.Fatal("request omitted selected account credentials")
			}
		case "auth.openai.com":
		default:
			t.Fatalf("unexpected upstream host %q", request.URL.Host)
		}
		return respond(request)
	})
}

func writeMockAuthFile(t *testing.T, name, accountID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	contents := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"test-refresh","account_id":%q}}`, accountID+"-access", accountID)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runMockAccountCLI(t *testing.T, arguments []string, input string, respond mockAccountTransport) (int, string, string) {
	t.Helper()
	var output, diagnostics bytes.Buffer
	options, err := parseCommandLine(arguments, &diagnostics)
	if err != nil {
		t.Fatalf("parse account command: %v\n%s", err, &diagnostics)
	}
	client := &http.Client{Transport: respond}
	code := runAccountCommand(t.Context(), options, client, strings.NewReader(input), &output, &diagnostics)
	return code, output.String(), diagnostics.String()
}
