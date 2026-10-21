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

// EnsureFresh performs proactive refresh, coalesced by the credential mutex.
func (c *Credential) EnsureFresh(ctx context.Context, config RefreshConfig) error {
	config = config.normalized()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.needsRefresh(config.Now()) {
		return nil
	}
	return c.refreshLocked(ctx, config)
}

// Refresh forces one refresh. It is used for a 401 before any client bytes are written.
func (c *Credential) Refresh(ctx context.Context, config RefreshConfig) error {
	config = config.normalized()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshLocked(ctx, config)
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
