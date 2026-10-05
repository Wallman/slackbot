// Package appstoreconnect provides a minimal client for the App Store Connect
// API, authenticated via JWT (ES256) as described at
// https://developer.apple.com/documentation/appstoreconnectapi/generating_tokens_for_api_requests
package appstoreconnect

import (
	"bufio"
	"compress/gzip"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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

func (c *Client) getDailyDownloadCount(vendorNumber, appleID string, reportDate time.Time) (downloads int, found bool, err error) {
	tok, err := c.token()
	if err != nil {
		return 0, false, fmt.Errorf("generate token: %w", err)
	}

	q := url.Values{}
	q.Set("filter[reportType]", "SALES")
	q.Set("filter[reportSubType]", "SUMMARY")
	q.Set("filter[frequency]", "DAILY")
	q.Set("filter[reportDate]", reportDate.Format("2006-01-02"))
	q.Set("filter[vendorNumber]", vendorNumber)

	req, err := http.NewRequest(http.MethodGet, baseURL+"/salesReports?"+q.Encode(), nil)
	if err != nil {
		return 0, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/a-gzip")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil // report not generated yet for this date
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return 0, false, fmt.Errorf("sales report error (status %d): %s", resp.StatusCode, string(body))
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, false, fmt.Errorf("ungzip report: %w", err)
	}
	defer gz.Close()

	var header []string
	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	colIndex := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		return -1
	}
	total := 0
	for scanner.Scan() {
		cols := strings.Split(scanner.Text(), "\t")
		if header == nil {
			header = cols
			continue
		}
		appleIDIdx, unitsIdx := colIndex("Apple Identifier"), colIndex("Units")
		productTypeIdx := colIndex("Product Type Identifier")
		if appleIDIdx < 0 || unitsIdx < 0 || productTypeIdx < 0 || appleIDIdx >= len(cols) || cols[appleIDIdx] != appleID {
			continue
		}
		switch cols[productTypeIdx] {
		case "1", "1F": // paid/free app download — the only real "downloads"
		default: // e.g. "3"/"3F" updates, "7"/"7F" in-app purchases
			continue
		}
		units, _ := strconv.Atoi(cols[unitsIdx])
		total += units
		found = true
	}
	if err := scanner.Err(); err != nil {
		return 0, false, fmt.Errorf("read report: %w", err)
	}
	return total, found, nil
}

func (c *Client) GetDownloadsTrend(vendorNumber, appleID string, days int) (string, error) {
	var sb strings.Builder
	sb.WriteString("Date\tDownloads\n")

	today := time.Now().UTC()
	for i := days; i >= 1; i-- {
		date := today.AddDate(0, 0, -i)
		downloads, found, err := c.getDailyDownloadCount(vendorNumber, appleID, date)
		if err != nil {
			sb.WriteString(fmt.Sprintf("%s\terror: %v\n", date.Format("2006-01-02"), err))
			continue
		}
		if !found {
			sb.WriteString(fmt.Sprintf("%s\tno data\n", date.Format("2006-01-02")))
			continue
		}
		sb.WriteString(fmt.Sprintf("%s\t%d\n", date.Format("2006-01-02"), downloads))
	}
	return sb.String(), nil
}
