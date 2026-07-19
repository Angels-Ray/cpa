package xai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	log "github.com/sirupsen/logrus"
)

// TokenStorage stores xAI OAuth credentials on disk.
type TokenStorage struct {
	Type          string `json:"type"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	IDToken       string `json:"id_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	ExpiresIn     int    `json:"expires_in,omitempty"`
	Expire        string `json:"expired,omitempty"`
	LastRefresh   string `json:"last_refresh,omitempty"`
	Email         string `json:"email,omitempty"`
	Subject       string `json:"sub,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	AuthKind      string `json:"auth_kind,omitempty"`

	Metadata map[string]any `json:"-"`
}

// SetMetadata allows the token store to merge status fields before saving.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes xAI credentials to a JSON auth file.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	if errMkdirAll := os.MkdirAll(filepath.Dir(authFilePath), 0o700); errMkdirAll != nil {
		return fmt.Errorf("xai token storage: create directory: %w", errMkdirAll)
	}
	file, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("xai token storage: create token file: %w", err)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("xai token storage: close token file error: %v", errClose)
		}
	}()

	data := make(map[string]any, 22)
	data["type"] = "xai"
	data["auth_kind"] = "oauth"
	if ts.AccessToken != "" {
		data["access_token"] = ts.AccessToken
	}
	if ts.RefreshToken != "" {
		data["refresh_token"] = ts.RefreshToken
	}
	if ts.IDToken != "" {
		data["id_token"] = ts.IDToken
	}
	if ts.TokenType != "" {
		data["token_type"] = ts.TokenType
	}
	if ts.ExpiresIn > 0 {
		data["expires_in"] = ts.ExpiresIn
	}
	if ts.Expire != "" {
		data["expired"] = ts.Expire
	}
	if ts.LastRefresh != "" {
		data["last_refresh"] = ts.LastRefresh
	}
	if ts.Email != "" {
		data["email"] = ts.Email
	}
	if ts.Subject != "" {
		data["sub"] = ts.Subject
	}
	if ts.BaseURL != "" {
		data["base_url"] = ts.BaseURL
	}
	if ts.RedirectURI != "" {
		data["redirect_uri"] = ts.RedirectURI
	}
	if ts.TokenEndpoint != "" {
		data["token_endpoint"] = ts.TokenEndpoint
	}
	for k, v := range ts.Metadata {
		data[k] = v
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("xai token storage: write token file: %w", err)
	}
	return nil
}

// CredentialFileName returns the filename used for xAI credentials.
func CredentialFileName(email, subject string) string {
	email = sanitizeFileSegment(email)
	if email != "" {
		return fmt.Sprintf("xai-%s.json", email)
	}
	subject = sanitizeFileSegment(subject)
	if subject != "" {
		return fmt.Sprintf("xai-%s.json", subject)
	}
	return fmt.Sprintf("xai-%d.json", time.Now().UnixMilli())
}

func sanitizeFileSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '@' || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
