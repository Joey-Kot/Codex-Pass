package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestLoadAllDeduplicatesFilesInInputOrder(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.json")
	second := filepath.Join(directory, "second.json")
	contents := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"r","account_id":"acct"}}`)
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{first, first}
	if hardlink := filepath.Join(directory, "hardlink.json"); os.Link(first, hardlink) == nil {
		paths = append(paths, hardlink)
	}
	if symlink := filepath.Join(directory, "symlink.json"); os.Symlink(first, symlink) == nil {
		paths = append([]string{symlink}, paths...)
	}
	paths = append(paths, second)
	credentials, err := LoadAll(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) != 2 || credentials[0].Path() != paths[0] || credentials[1].Path() != second {
		t.Fatalf("loaded %d credentials, want the first path and the distinct copy", len(credentials))
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
	var credentials [2]*Credential
	for index := range credentials {
		credential, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		credentials[index] = credential
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
	for index := range 32 {
		credential := credentials[index%len(credentials)]
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
	for _, credential := range credentials {
		if got := credential.Snapshot().Token; got != "new-access" {
			t.Fatalf("access token = %q", got)
		}
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

func TestAuthRefreshLockWaitRespectsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json.codex-pass.lock")
	release, err := lockAuthFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	secondRelease, err := lockAuthFile(ctx, path)
	if err == nil {
		secondRelease()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock attempt returned %v, want deadline exceeded", err)
	}
}

func TestRefreshedFlattenedFileKeepsAliasesReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	contents := []byte(`{"type":"codex","access_token":"old-access","accessToken":"old-access","refresh_token":"old-refresh","refreshToken":"old-refresh","account_id":"acct"}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	credential, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
	}))
	defer server.Close()
	config := RefreshConfig{Endpoint: server.URL, Client: server.Client()}
	if err := credential.Refresh(t.Context(), config, "old-access"); err != nil {
		t.Fatal(err)
	}
	updated, err := Load(path)
	if err != nil || updated.accessToken != "new-access" || updated.refreshToken != "new-refresh" {
		t.Fatalf("could not reload rotated token aliases: %v", err)
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
