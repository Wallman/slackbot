// Package cloudflare provides a minimal client for Cloudflare's GraphQL
// Analytics API (https://developers.cloudflare.com/analytics/graphql-api/).
package cloudflare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const graphqlURL = "https://api.cloudflare.com/client/v4/graphql"

// Client calls the Cloudflare GraphQL Analytics API.
type Client struct {
	APIToken   string
	ZoneID     string
	HTTPClient *http.Client
}

// NewClient creates a new Cloudflare Analytics client.
func NewClient(apiToken, zoneID string) *Client {
	return &Client{
		APIToken:   apiToken,
		ZoneID:     zoneID,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// Query runs an arbitrary GraphQL query/variables pair against Cloudflare's
// Analytics API and returns the raw JSON response body.
func (c *Client) Query(query string, variables map[string]any) (string, error) {
	reqBody, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, graphqlURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIToken)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("cloudflare error (status %d): %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// GetZoneRequestTotals returns total requests/bandwidth/threats for the zone
// over the last N days, as a smoke test of Analytics access.
func (c *Client) GetZoneRequestTotals(days int) (string, error) {
	query := `
	query ZoneAnalytics($zoneTag: string, $since: Time, $until: Time) {
	  viewer {
	    zones(filter: {zoneTag: $zoneTag}) {
	      httpRequests1dGroups(limit: 100, filter: {date_geq: $since, date_leq: $until}) {
	        dimensions { date }
	        sum { requests bytes threats }
	      }
	    }
	  }
	}`
	now := time.Now().UTC()
	since := now.AddDate(0, 0, -days).Format("2006-01-02")
	until := now.Format("2006-01-02")

	variables := map[string]any{
		"zoneTag": c.ZoneID,
		"since":   since,
		"until":   until,
	}
	return c.Query(query, variables)
}
