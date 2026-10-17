package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type TOTPClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewTOTPClient(baseURL, apiKey string) *TOTPClient {
	return &TOTPClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

type totpRegisterRequest struct {
	Service string `json:"service"`
	Secret  string `json:"secret"`
}

type totpCodeResponse struct {
	Service   string `json:"service"`
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"`
	Generated string `json:"generated_at"`
}

type totpListResponse struct {
	Services []string `json:"services"`
}

func (c *TOTPClient) Register(ctx context.Context, service, secret string) error {
	body, _ := json.Marshal(totpRegisterRequest{
		Service: service,
		Secret:  secret,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/register", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("auth/totp: create register request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth/totp: register request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("auth/totp: register failed (%d): %s", resp.StatusCode, string(raw))
	}

	return nil
}

func (c *TOTPClient) GenerateCode(ctx context.Context, service string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/code?service="+service, nil)
	if err != nil {
		return "", 0, fmt.Errorf("auth/totp: create code request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("auth/totp: code request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", 0, fmt.Errorf("auth/totp: code failed (%d): %s", resp.StatusCode, string(raw))
	}

	var result totpCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", 0, fmt.Errorf("auth/totp: decode response: %w", err)
	}

	return result.Code, result.ExpiresIn, nil
}

func (c *TOTPClient) WaitForNextPeriod(ctx context.Context) error {
	_, expiresIn, err := c.GenerateCode(ctx, "__probe__")
	if err != nil {
		expiresIn = 30
	}

	if expiresIn < 3 {
		expiresIn += 30
	}

	timer := time.NewTimer(time.Duration(expiresIn) * time.Second)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *TOTPClient) ListServices(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/list", nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result totpListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Services, nil
}

func (c *TOTPClient) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth/totp: health check: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth/totp: health check failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *TOTPClient) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
}
