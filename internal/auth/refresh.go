package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RefreshConfig is injectable so unit tests never contact an OAuth endpoint.
type RefreshConfig struct {
	Endpoint string
	ClientID string
	Client   *http.Client
	Now      func() time.Time
}

func (c RefreshConfig) normalized() RefreshConfig {
	if c.Endpoint == "" {
		c.Endpoint = RefreshEndpoint
	}
	if c.ClientID == "" {
		c.ClientID = ClientID
	}
	if c.Client == nil {
		c.Client = http.DefaultClient
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// EnsureFresh checks the cached credential before consulting the file on refresh.
func (c *Credential) EnsureFresh(ctx context.Context, config RefreshConfig) error {
	config = config.normalized()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.needsRefresh(config.Now()) {
		return nil
	}
	return c.refreshFromFile(ctx, config, "")
}

// Refresh handles a 401, reusing a fresh token if the rejected token was replaced.
func (c *Credential) Refresh(ctx context.Context, config RefreshConfig, rejectedToken string) error {
	config = config.normalized()
	c.mu.Lock()
	defer c.mu.Unlock()
	if rejectedToken == "" {
		rejectedToken = c.accessToken
	}
	return c.refreshFromFile(ctx, config, rejectedToken)
}

// refreshFromFile holds the credential mutex and locks the file across processes.
func (c *Credential) refreshFromFile(ctx context.Context, config RefreshConfig, rejectedToken string) error {
	release, err := lockAuthFile(ctx, c.resolvedPath+".codex-pass.lock")
	if err != nil {
		return err
	}
	defer release()

	latest, err := loadResolvedCredential(c.path, c.resolvedPath)
	if err != nil {
		return err
	}
	if latest.accountID != c.accountID {
		return fmt.Errorf("account_id changed in auth file %q", c.path)
	}
	if latest.needsRefresh(config.Now()) || latest.accessToken == rejectedToken {
		if err := latest.refreshLocked(ctx, config); err != nil {
			return err
		}
	}
	c.format, c.root = latest.format, latest.root
	c.accessToken, c.refreshToken = latest.accessToken, latest.refreshToken
	c.lastRefresh = latest.lastRefresh
	return nil
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

func (c *Credential) refreshLocked(ctx context.Context, config RefreshConfig) error {
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     config.ClientID,
		"refresh_token": c.refreshToken,
	})
	if err != nil {
		return fmt.Errorf("encode token refresh request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create token refresh request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := config.Client.Do(request)
	if err != nil {
		return fmt.Errorf("send token refresh request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		return fmt.Errorf("token refresh returned HTTP %d: %s", response.StatusCode, redactRefreshError(string(body)))
	}
	var refreshed refreshResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&refreshed); err != nil {
		return fmt.Errorf("parse token refresh response: %w", err)
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		return fmt.Errorf("token refresh response did not contain access_token")
	}
	if err := c.persistLocked(refreshed.AccessToken, refreshed.RefreshToken, refreshed.IDToken, config.Now()); err != nil {
		return fmt.Errorf("persist refreshed token: %w", err)
	}
	return nil
}

func redactRefreshError(value string) string {
	// Error bodies are not authoritative diagnostics and can echo a supplied token.
	// Do not expose them through an error path.
	if strings.TrimSpace(value) == "" {
		return "no response body"
	}
	return "OAuth endpoint rejected the refresh request"
}
