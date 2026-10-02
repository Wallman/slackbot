// Package appstoreconnect provides a minimal client for the App Store Connect
// API, authenticated via JWT (ES256) as described at
// https://developer.apple.com/documentation/appstoreconnectapi/generating_tokens_for_api_requests
package appstoreconnect

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const baseURL = "https://api.appstoreconnect.apple.com/v1"

// Client authenticates and calls the App Store Connect API.
type Client struct {
	IssuerID       string
	KeyID          string
	PrivateKeyPath string
	HTTPClient     *http.Client
}

// NewClient creates a new App Store Connect API client.
func NewClient(issuerID, keyID, privateKeyPath string) *Client {
	return &Client{
		IssuerID:       issuerID,
		KeyID:          keyID,
		PrivateKeyPath: privateKeyPath,
		HTTPClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// token generates a short-lived signed JWT for authenticating requests.
func (c *Client) token() (string, error) {
	keyData, err := os.ReadFile(c.PrivateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read private key: %w", err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return "", fmt.Errorf("invalid PEM in private key file")
	}
	privKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss": c.IssuerID,
		"iat": now.Unix(),
		"exp": now.Add(19 * time.Minute).Unix(), // ASC tokens must be <= 20 min
		"aud": "appstoreconnect-v1",
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["kid"] = c.KeyID

	return t.SignedString(privKey)
}

// get performs an authenticated GET against the App Store Connect API and
// returns the raw response body.
func (c *Client) get(path string) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}

	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("app store connect error (status %d): %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// GetApps returns the raw JSON:API response listing apps visible to this key.
// Useful as a smoke test and for discovering app IDs.
func (c *Client) GetApps() (string, error) {
	body, err := c.get("/apps?limit=50")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// GetAppInfo returns raw JSON:API details for a single app by ID.
func (c *Client) GetAppInfo(appID string) (string, error) {
	body, err := c.get(fmt.Sprintf("/apps/%s", appID))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
