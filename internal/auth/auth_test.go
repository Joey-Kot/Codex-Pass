package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadOfficialAndFlattenedFormats(t *testing.T) {
	directory := t.TempDir()
	officialPath := filepath.Join(directory, "official.json")
	if err := os.WriteFile(officialPath, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","account_id":"acct","unknown":"kept"},"unknown_top":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	official, err := Load(officialPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := official.Snapshot(); got.Format != "official" || got.Token != "a" || got.AccountID != "acct" {
		t.Fatalf("unexpected official credential: %#v", got)
	}
	flatPath := filepath.Join(directory, "flat.json")
	if err := os.WriteFile(flatPath, []byte(`{"type":"codex","accessToken":"a","refreshToken":"r","chatgpt_account_id":"acct"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	flat, err := Load(flatPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := flat.Snapshot(); got.Format != "flattened" || got.Token != "a" || got.AccountID != "acct" {
		t.Fatalf("unexpected flattened credential: %#v", got)
	}
}

func TestLoadRejectsUnknownOrIncompleteFormats(t *testing.T) {
	directory := t.TempDir()
	for name, content := range map[string]string{
		"unknown":    `{"access_token":"a"}`,
		"incomplete": `{"auth_mode":"chatgpt","tokens":{"access_token":"a"}}`,
	} {
		path := filepath.Join(directory, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("%s unexpectedly loaded", name)
		}
	}
}

func TestRefreshIsCoalescedAndPreservesUnknownFields(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	expired := testJWT(time.Now().Add(-time.Hour))
	content := `{"auth_mode":"chatgpt","last_refresh":"2020-01-01T00:00:00Z","unknown_top":{"keep":true},"tokens":{"access_token":"` + expired + `","refresh_token":"old-refresh","account_id":"acct","unknown":"kept"}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	credential, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		var got map[string]string
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got["grant_type"] != "refresh_token" || got["refresh_token"] != "old-refresh" || got["client_id"] != ClientID {
			t.Errorf("unexpected refresh payload: %#v", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","id_token":"new-id"}`))
	}))
	defer server.Close()
	config := RefreshConfig{Endpoint: server.URL, Client: server.Client()}
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := credential.EnsureFresh(context.Background(), config); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if got := credential.Snapshot().Token; got != "new-access" {
		t.Fatalf("access token = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file permissions = %o, want 600", info.Mode().Perm())
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(stored, &value); err != nil {
		t.Fatal(err)
	}
	if value["unknown_top"].(map[string]any)["keep"] != true || value["tokens"].(map[string]any)["unknown"] != "kept" {
		t.Fatalf("unknown fields were not preserved: %s", stored)
	}
}

func TestPoolSelectionIsStable(t *testing.T) {
	credentials := []*Credential{{accessToken: "a", refreshToken: "r", accountID: "a"}, {accessToken: "b", refreshToken: "r", accountID: "b"}}
	pool, err := NewPool(credentials, RefreshConfig{})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := pool.Select("cache-key", "session-one")
	second, _ := pool.Select("cache-key", "session-two")
	if first != second {
		t.Fatalf("same cache key selected %d then %d", first, second)
	}
	if index, _ := pool.Select("", "session-one"); index < 0 || index >= 2 {
		t.Fatalf("bad session selection: %d", index)
	}
}

func testJWT(expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]int64{"exp": expiry.Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
