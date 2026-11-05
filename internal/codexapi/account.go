package codexapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"codex-pass/internal/auth"

	"github.com/google/uuid"
)

const (
	BackendURL          = "https://chatgpt.com/backend-api"
	CodexModelsPath     = "/codex/models"
	ModelsClientVersion = "0.166.0"
	usagePath           = "/wham/usage"
	resetCreditsPath    = "/wham/rate-limit-reset-credits"
	maxResponseBytes    = 8 << 20
)

// AccountClient uses the authentication file selected for a one-shot command.
type AccountClient struct {
	credential *auth.Credential
	httpClient *http.Client
	refresh    auth.RefreshConfig
}

func NewAccountClient(credential *auth.Credential, client *http.Client) *AccountClient {
	// Keep credentials on the intended host and prevent redirects from replaying a redemption.
	noRedirects := *client
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &AccountClient{
		credential: credential, httpClient: &noRedirects,
		refresh: auth.RefreshConfig{Client: &noRedirects},
	}
}

type Usage struct {
	Plan             string    `json:"plan_type"`
	RateLimit        RateLimit `json:"rate_limit"`
	CodeReviewLimit  RateLimit `json:"code_review_rate_limit"`
	AdditionalLimits []struct {
		Name           string    `json:"limit_name"`
		MeteredFeature string    `json:"metered_feature"`
		RateLimit      RateLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
}

type RateLimit struct {
	Primary   *RateLimitWindow `json:"primary_window"`
	Secondary *RateLimitWindow `json:"secondary_window"`
}

type RateLimitWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	Seconds     int      `json:"limit_window_seconds"`
	ResetAt     int64    `json:"reset_at"`
}

func (c *AccountClient) Usage(ctx context.Context) (Usage, error) {
	var usage Usage
	err := c.requestJSON(ctx, http.MethodGet, usagePath, nil, &usage)
	return usage, err
}

type ResetCredit struct {
	ID              string    `json:"id"`
	Type            string    `json:"reset_type"`
	Title           string    `json:"title"`
	Status          string    `json:"status"`
	SupportedByPlan *bool     `json:"is_supported_by_plan"`
	GrantedAt       time.Time `json:"granted_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func (c *AccountClient) ResetCredits(ctx context.Context) (int, []ResetCredit, error) {
	var response struct {
		AvailableCount *int          `json:"available_count"`
		Credits        []ResetCredit `json:"credits"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, resetCreditsPath, nil, &response); err != nil {
		return 0, nil, err
	}
	if response.AvailableCount == nil || *response.AvailableCount < 0 {
		return 0, nil, fmt.Errorf("reset credit response did not contain a valid available_count")
	}
	return *response.AvailableCount, response.Credits, nil
}

type ResetResult struct {
	Code         string `json:"code"`
	WindowsReset int    `json:"windows_reset"`
}

func (c *AccountClient) ConsumeResetCredit(ctx context.Context, creditID string) (ResetResult, error) {
	if creditID == "" {
		return ResetResult{}, fmt.Errorf("the selected reset credit has no ID")
	}
	redeemID, err := uuid.NewRandom()
	if err != nil {
		return ResetResult{}, fmt.Errorf("generate reset request ID: %w", err)
	}
	body, err := json.Marshal(map[string]string{"redeem_request_id": redeemID.String(), "credit_id": creditID})
	if err != nil {
		return ResetResult{}, err
	}
	var result ResetResult
	err = c.requestJSON(ctx, http.MethodPost, resetCreditsPath+"/consume", body, &result)
	return result, err
}

type Model struct {
	Slug         string   `json:"slug"`
	DisplayName  string   `json:"display_name"`
	Title        string   `json:"title"`
	Visibility   string   `json:"visibility"`
	EnabledTools []string `json:"enabled_tools"`
}

func (c *AccountClient) CodexModels(ctx context.Context, version string) ([]Model, error) {
	if version == "" {
		version = ModelsClientVersion
	}
	return c.models(ctx, CodexModelsPath+"?client_version="+url.QueryEscape(version))
}

func (c *AccountClient) ChatGPTModels(ctx context.Context) ([]Model, error) {
	return c.models(ctx, "/models")
}

func (c *AccountClient) models(ctx context.Context, endpoint string) ([]Model, error) {
	var response struct {
		Models []Model `json:"models"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, err
	}
	if response.Models == nil {
		return nil, fmt.Errorf("model catalog response did not contain a models array")
	}
	return response.Models, nil
}

func (c *AccountClient) requestJSON(ctx context.Context, method, endpoint string, body []byte, output any) error {
	if err := c.credential.EnsureFresh(ctx, c.refresh); err != nil {
		return err
	}
	var sentToken string
	send := func() (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, method, BackendURL+endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		snapshot := c.credential.Snapshot()
		sentToken = snapshot.Token
		request.Header.Set("Authorization", "Bearer "+sentToken)
		request.Header.Set("ChatGPT-Account-ID", snapshot.AccountID)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Originator", "codex-pass")
		request.Header.Set("User-Agent", "codex-pass/0.1")
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		return c.httpClient.Do(request)
	}
	response, err := send()
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		if err := c.credential.Refresh(ctx, c.refresh, sentToken); err != nil {
			return err
		}
		// A redemption keeps both the credit ID and the request ID on this retry.
		response, err = send()
		if err != nil {
			return err
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned HTTP %d", method, endpoint, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read %s response: %w", endpoint, err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("%s response exceeds %d bytes", endpoint, maxResponseBytes)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	return nil
}
