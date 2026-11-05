// Package auth loads and maintains the small set of Codex OAuth file formats
// supported by codex-pass. It deliberately rejects unknown formats: guessing a
// credential layout is unsafe and makes diagnostics unreliable.
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	RefreshEndpoint = "https://auth.openai.com/oauth/token"
	// CLIENT_ID is the public Codex CLI OAuth client identifier used by the
	// official refresh implementation.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// Credential is one independently refreshable Codex account.
// Fields accessed outside this package are exposed only through Snapshot.
type Credential struct {
	path         string
	resolvedPath string

	mu           sync.Mutex
	format       string
	root         map[string]json.RawMessage
	accessToken  string
	refreshToken string
	accountID    string
	lastRefresh  time.Time
}

// Snapshot is a non-secret metadata and token snapshot for creating an
// upstream request. Callers must never log Token.
type Snapshot struct {
	Token     string
	AccountID string
	Path      string
	Format    string
}

func (c *Credential) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{
		Token: c.accessToken, AccountID: c.accountID, Path: c.path, Format: c.format,
	}
}

func (c *Credential) Path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

func (c *Credential) Format() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.format
}

// Load validates a Codex auth file without emitting any credential material.
func Load(path string) (*Credential, error) {
	resolved, err := resolveAuthPath(path)
	if err != nil {
		return nil, err
	}
	return loadResolvedCredential(path, resolved)
}

func resolveAuthPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("auth file path is empty")
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve auth file %q: %w", path, err)
	}
	resolved, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve absolute auth file %q: %w", path, err)
	}
	return resolved, nil
}

func loadResolvedCredential(path, resolved string) (*Credential, error) {
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read auth file %q: %w", path, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse auth file %q as JSON: %w", path, err)
	}
	if root == nil {
		return nil, fmt.Errorf("auth file %q must contain a JSON object", path)
	}

	var credential *Credential
	if mode, ok := stringField(root, "auth_mode"); ok && strings.EqualFold(mode, "chatgpt") {
		credential, err = parseOfficial(path, root)
	} else if typ, ok := stringField(root, "type"); ok && strings.EqualFold(typ, "codex") {
		credential, err = parseFlattened(path, root)
	} else {
		return nil, fmt.Errorf("auth file %q has an unknown format: expected official auth_mode=chatgpt with tokens, or flattened type=codex", path)
	}
	if err != nil {
		return nil, err
	}
	credential.resolvedPath = resolved
	return credential, nil
}

// LoadAll preserves the first supplied path for each distinct file.
func LoadAll(paths []string) ([]*Credential, error) {
	if len(paths) == 0 {
		return nil, errors.New("at least one Codex auth file is required")
	}
	credentials := make([]*Credential, 0, len(paths))
	seenPaths := make(map[string]struct{}, len(paths))
	seenFiles := make([]os.FileInfo, 0, len(paths))
	for _, path := range paths {
		resolved, err := resolveAuthPath(path)
		if err != nil {
			return nil, err
		}
		if _, seen := seenPaths[resolved]; seen {
			continue
		}
		seenPaths[resolved] = struct{}{}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("stat auth file %q: %w", path, err)
		}
		if slices.ContainsFunc(seenFiles, func(previous os.FileInfo) bool { return os.SameFile(info, previous) }) {
			continue
		}
		credential, err := loadResolvedCredential(path, resolved)
		if err != nil {
			return nil, err
		}
		seenFiles = append(seenFiles, info)
		credentials = append(credentials, credential)
	}
	return credentials, nil
}

func parseOfficial(path string, root map[string]json.RawMessage) (*Credential, error) {
	var tokens map[string]json.RawMessage
	rawTokens, ok := root["tokens"]
	if !ok || json.Unmarshal(rawTokens, &tokens) != nil || tokens == nil {
		return nil, fmt.Errorf("official auth file %q requires an object at tokens", path)
	}
	access, accessOK := stringField(tokens, "access_token")
	refresh, refreshOK := stringField(tokens, "refresh_token")
	account, accountOK := stringField(tokens, "account_id")
	if !accessOK || !refreshOK || !accountOK || strings.TrimSpace(access) == "" || strings.TrimSpace(refresh) == "" || strings.TrimSpace(account) == "" {
		return nil, fmt.Errorf("official auth file %q requires non-empty tokens.access_token, tokens.refresh_token, and tokens.account_id", path)
	}
	return &Credential{
		path: path, format: "official", root: root, accessToken: access, refreshToken: refresh, accountID: account,
		lastRefresh: parseTimeField(root, "last_refresh"),
	}, nil
}

func parseFlattened(path string, root map[string]json.RawMessage) (*Credential, error) {
	access, accessOK, err := oneOfString(root, "access_token", "accessToken")
	if err != nil {
		return nil, fmt.Errorf("flattened auth file %q: %w", path, err)
	}
	refresh, refreshOK, err := oneOfString(root, "refresh_token", "refreshToken")
	if err != nil {
		return nil, fmt.Errorf("flattened auth file %q: %w", path, err)
	}
	account, accountOK, err := oneOfString(root, "account_id", "chatgpt_account_id", "accountId")
	if err != nil {
		return nil, fmt.Errorf("flattened auth file %q: %w", path, err)
	}
	if !accessOK || !refreshOK || !accountOK || strings.TrimSpace(access) == "" || strings.TrimSpace(refresh) == "" || strings.TrimSpace(account) == "" {
		return nil, fmt.Errorf("flattened auth file %q requires non-empty access_token, refresh_token, and account_id (chatgpt_account_id is also accepted)", path)
	}
	return &Credential{
		path: path, format: "flattened", root: root, accessToken: access, refreshToken: refresh, accountID: account,
		lastRefresh: parseTimeField(root, "last_refresh"),
	}, nil
}

func stringField(values map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := values[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func oneOfString(values map[string]json.RawMessage, names ...string) (string, bool, error) {
	var result string
	found := false
	for _, name := range names {
		value, ok := stringField(values, name)
		if !ok {
			continue
		}
		if found && value != result {
			return "", false, fmt.Errorf("conflicting values for %s", strings.Join(names, ", "))
		}
		result, found = value, true
	}
	return result, found, nil
}

func parseTimeField(values map[string]json.RawMessage, key string) time.Time {
	value, ok := stringField(values, key)
	if !ok {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func parseJWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Expiration json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil || claims.Expiration == "" {
		return time.Time{}, false
	}
	seconds, err := claims.Expiration.Int64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

func (c *Credential) needsRefresh(now time.Time) bool {
	if expiry, ok := parseJWTExpiry(c.accessToken); ok {
		return !expiry.After(now.Add(5 * time.Minute))
	}
	return !c.lastRefresh.IsZero() && c.lastRefresh.Before(now.Add(-8*24*time.Hour))
}

func marshalString(value string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func updateTokenAliases(root map[string]json.RawMessage, value string, names ...string) {
	raw := marshalString(value)
	for _, name := range names {
		if _, exists := stringField(root, name); exists {
			root[name] = raw
		}
	}
}

func (c *Credential) persistLocked(access, refresh, idToken string, now time.Time) error {
	if c.format == "official" {
		var tokens map[string]json.RawMessage
		if err := json.Unmarshal(c.root["tokens"], &tokens); err != nil || tokens == nil {
			return errors.New("cannot update malformed official tokens object")
		}
		tokens["access_token"] = marshalString(access)
		if refresh != "" {
			tokens["refresh_token"] = marshalString(refresh)
		}
		if idToken != "" {
			tokens["id_token"] = marshalString(idToken)
		}
		rawTokens, err := json.Marshal(tokens)
		if err != nil {
			return fmt.Errorf("encode refreshed token object: %w", err)
		}
		c.root["tokens"] = rawTokens
	} else {
		updateTokenAliases(c.root, access, "access_token", "accessToken")
		if refresh != "" {
			updateTokenAliases(c.root, refresh, "refresh_token", "refreshToken")
		}
		if idToken != "" {
			c.root["id_token"] = marshalString(idToken)
		}
	}
	c.root["last_refresh"] = marshalString(now.UTC().Format(time.RFC3339))
	encoded, err := json.MarshalIndent(c.root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode auth file: %w", err)
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(c.resolvedPath)
	temporary, err := os.CreateTemp(directory, ".codex-pass-auth-*")
	if err != nil {
		return fmt.Errorf("create temporary auth file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary auth file permissions: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary auth file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary auth file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary auth file: %w", err)
	}
	if err := os.Rename(temporaryName, c.resolvedPath); err != nil {
		return fmt.Errorf("atomically replace auth file: %w", err)
	}
	c.accessToken = access
	if refresh != "" {
		c.refreshToken = refresh
	}
	c.lastRefresh = now
	return nil
}
