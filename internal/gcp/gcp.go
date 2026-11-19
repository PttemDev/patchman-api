// Package gcp resolves a Google Cloud credential to an authenticated HTTP
// client and talks to the REST APIs PatchMan uses: Cloud Storage, Pub/Sub,
// Cloud Logging, GKE (container) and Resource Manager (projects). REST, not
// the gRPC SDKs, keeps the sidecar binary small (RFC ADR-2).
package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Scopes per service (requested only for what is used).
const (
	ScopeReadOnly     = "https://www.googleapis.com/auth/cloud-platform.read-only"
	ScopeCloud        = "https://www.googleapis.com/auth/cloud-platform"
	ScopeStorageRead  = "https://www.googleapis.com/auth/devstorage.read_only"
	ScopeStorageWrite = "https://www.googleapis.com/auth/devstorage.read_write"
	ScopePubSub       = "https://www.googleapis.com/auth/pubsub"
	ScopeLoggingRead  = "https://www.googleapis.com/auth/logging.read"
	ScopeBigQuery     = "https://www.googleapis.com/auth/bigquery"
)

// Service API roots. Override points them all at one fake / emulator:
// <override>/storage/v1, <override>/pubsub/v1, … (GCP_API_BASE).
var override = ""

// UseEndpoint sets the override ("" restores Google).
func UseEndpoint(base string) { override = strings.TrimRight(base, "/") }

// Overridden reports whether a fake endpoint is in use (no Google auth then).
func Overridden() bool { return override != "" }

func base(service, real string) string {
	if override != "" {
		return override + "/" + service
	}
	return real
}

func StorageBase() string   { return base("storage", "https://storage.googleapis.com/storage/v1") }
func UploadBase() string    { return base("upload", "https://storage.googleapis.com/upload/storage/v1") }
func PubSubBase() string    { return base("pubsub", "https://pubsub.googleapis.com/v1") }
func LoggingBase() string   { return base("logging", "https://logging.googleapis.com/v2") }
func ContainerBase() string { return base("container", "https://container.googleapis.com/v1") }
func ProjectsBase() string  { return base("crm", "https://cloudresourcemanager.googleapis.com/v1") }

// OAuthApp is the "Connect Google Cloud" OAuth client (the sign-in client,
// or the External one for other domains — RFC ADR-4).
type OAuthApp struct {
	ClientID, ClientSecret string
}

// Source describes a credential well enough to build a token source.
type Source struct {
	Kind   string // service_account | adc | oauth
	Secret string // key JSON / refresh token (decrypted)
	OAuth  OAuthApp
}

// TokenSource returns a refreshing token source for the scopes.
func TokenSource(ctx context.Context, s Source, scopes ...string) (oauth2.TokenSource, error) {
	if len(scopes) == 0 {
		scopes = []string{ScopeCloud}
	}
	switch s.Kind {
	case "service_account":
		if strings.TrimSpace(s.Secret) == "" {
			return nil, errors.New("the service account key is missing")
		}
		c, err := google.CredentialsFromJSON(ctx, []byte(s.Secret), scopes...)
		if err != nil {
			return nil, fmt.Errorf("service account key: %w", err)
		}
		return c.TokenSource, nil
	case "adc":
		c, err := google.FindDefaultCredentials(ctx, scopes...)
		if err != nil {
			return nil, fmt.Errorf("no gcloud credentials on this computer — run `gcloud auth application-default login`: %w", err)
		}
		return c.TokenSource, nil
	case "oauth":
		if s.OAuth.ClientID == "" {
			return nil, errors.New("Google sign-in isn't configured on this server, so a linked Google account can't be used")
		}
		cfg := &oauth2.Config{ClientID: s.OAuth.ClientID, ClientSecret: s.OAuth.ClientSecret, Endpoint: google.Endpoint, Scopes: scopes}
		return cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: s.Secret}), nil
	}
	return nil, fmt.Errorf("unknown credential kind %q", s.Kind)
}

// Client is an authenticated Google API client.
type Client struct {
	HTTP *http.Client
}

// NewClient builds a client for the scopes. Against a fake endpoint no
// Google auth is attempted.
func NewClient(ctx context.Context, s Source, scopes ...string) (*Client, error) {
	if Overridden() {
		return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
	}
	ts, err := TokenSource(ctx, s, scopes...)
	if err != nil {
		return nil, err
	}
	cl := oauth2.NewClient(context.Background(), ts)
	cl.Timeout = 60 * time.Second
	return &Client{HTTP: cl}, nil
}

// APIError is a non-2xx Google answer.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("google %d: %s", e.Status, e.Message) }

func apiError(status int, body []byte) error {
	var g struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &g) == nil && g.Error.Message != "" {
		msg = g.Error.Message
	}
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	switch status {
	case http.StatusUnauthorized:
		msg = "Google didn't accept the credential (expired or revoked): " + msg
	case http.StatusForbidden:
		msg = "The credential isn't allowed to do this (missing IAM role or API not enabled): " + msg
	}
	return &APIError{Status: status, Message: msg}
}

// Do sends a request; body (if any) is JSON-encoded; out (if any) decodes the answer.
func (c *Client) Do(ctx context.Context, method, endpoint string, q url.Values, body, out any) error {
	raw, _, err := c.DoRaw(ctx, method, endpoint, q, body, "", 32<<20)
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode google response: %w", err)
		}
	}
	return nil
}

// DoRaw is Do returning the raw body (capped at limit) and headers. A []byte
// body is sent as is with contentType.
func (c *Client) DoRaw(ctx context.Context, method, endpoint string, q url.Values, body any, contentType string, limit int64) ([]byte, http.Header, error) {
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(j)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	if resp.StatusCode/100 != 2 {
		return nil, resp.Header, apiError(resp.StatusCode, raw)
	}
	return raw, resp.Header, nil
}

// ── ADC discovery (desktop) ─────────────────────────────────────────────────

// ADCInfo is what `gcloud auth application-default login` left on this computer.
type ADCInfo struct {
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Type    string `json:"type,omitempty"`    // authorized_user | service_account | …
	Account string `json:"account,omitempty"` // service account email when known
	Project string `json:"project,omitempty"` // quota project / gcloud default project
}

// FindADC looks for Application Default Credentials without using them.
func FindADC() ADCInfo {
	path := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ADCInfo{}
	}
	var f struct {
		Type           string `json:"type"`
		ClientEmail    string `json:"client_email"`
		QuotaProjectID string `json:"quota_project_id"`
		ProjectID      string `json:"project_id"`
	}
	_ = json.Unmarshal(raw, &f)
	info := ADCInfo{Found: true, Path: path, Type: f.Type, Account: f.ClientEmail, Project: f.QuotaProjectID}
	if info.Project == "" {
		info.Project = f.ProjectID
	}
	if info.Project == "" {
		info.Project = gcloudDefaultProject()
	}
	return info
}

// gcloudDefaultProject reads `project` from gcloud's active configuration
// file (no gcloud binary is run).
func gcloudDefaultProject() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".config", "gcloud")
	active := "default"
	if b, err := os.ReadFile(filepath.Join(dir, "active_config")); err == nil && strings.TrimSpace(string(b)) != "" {
		active = strings.TrimSpace(string(b))
	}
	b, err := os.ReadFile(filepath.Join(dir, "configurations", "config_"+active))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok && strings.TrimSpace(k) == "project" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ServiceAccountInfo reads the email and project of a key JSON.
func ServiceAccountInfo(key string) (email, project string, err error) {
	var f struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		ProjectID   string `json:"project_id"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(key), &f); err != nil {
		return "", "", errors.New("the key isn't valid JSON")
	}
	if f.Type != "service_account" || f.ClientEmail == "" || f.PrivateKey == "" {
		return "", "", errors.New("this isn't a service account key (type service_account with client_email and private_key)")
	}
	return f.ClientEmail, f.ProjectID, nil
}
