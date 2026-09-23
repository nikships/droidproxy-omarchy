package auth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// SaveJunieAPIKey writes a Junie API key into the auth directory as
// junie.json, pretty-printed and 0600 (SettingsView.saveJunieApiKey). An
// empty key is a no-op, matching the Swift guard. Callers should trigger a
// rescan afterwards (Manager.CheckAuthStatus).
func SaveJunieAPIKey(apiKey string) error {
	if apiKey == "" {
		return nil
	}

	authDir := paths.AuthDir()
	filePath := filepath.Join(authDir, "junie.json")

	payload := map[string]any{
		"type":     "junie",
		"email":    "junie-user",
		"apiKey":   apiKey,
		"disabled": false,
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		return err
	}

	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return err
	}
	// 0600: the API key must not be readable by other users. Write directly
	// and chmod so an existing file's permissions are tightened too.
	if err := os.WriteFile(filePath, buf.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(filePath, 0o600); err != nil {
		return err
	}
	logx.Logf("[SettingsView] Saved Junie API Key with secure permissions to %s", filePath)
	return nil
}
