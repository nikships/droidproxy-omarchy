// Package usage tracks OAuth quota windows for Codex and Claude accounts
// (macOS OAuthUsageTracker).
package usage

import (
	"encoding/json"
	"os"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
)

// Window is one quota window. RemainingPercent/HasRemaining/ResetText are the
// fields the control API serializes (docs/control-api.md "usage").
type Window struct {
	Title            string     `json:"title"`
	RemainingPercent *float64   `json:"remainingPercent"`
	HasRemaining     bool       `json:"hasRemaining"`
	ResetText        string     `json:"resetText"`
	UsedPercent      *float64   `json:"-"`
	ResetDate        *time.Time `json:"-"`
}

// AccountUsage is the usage snapshot for one account.
type AccountUsage struct {
	ID           string     `json:"-"`
	Provider     string     `json:"provider"`
	ProviderName string     `json:"providerName"`
	Email        string     `json:"email"`
	Loading      bool       `json:"loading"`
	Error        string     `json:"error"`
	Windows      []Window   `json:"windows"`
	UpdatedAt    *time.Time `json:"-"`
}

// newWindow builds a Window from a used percentage, computing the
// remaining fields like the Swift remainingPercent property.
func newWindow(title string, usedPercent *float64, resetText string, resetDate *time.Time) Window {
	w := Window{Title: title, ResetText: resetText, ResetDate: resetDate}
	if usedPercent != nil {
		remaining := 100 - *usedPercent
		if remaining < 0 {
			remaining = 0
		}
		w.UsedPercent = usedPercent
		w.RemainingPercent = &remaining
		w.HasRemaining = remaining > 0
	}
	return w
}

func loadingPlaceholder(account auth.Account, provider auth.ServiceType) AccountUsage {
	return AccountUsage{
		ID:           account.ID,
		Provider:     string(provider),
		ProviderName: provider.DisplayName(),
		Email:        account.DisplayName(),
		Loading:      true,
	}
}

func successAccount(account auth.Account, windows []Window) AccountUsage {
	now := time.Now()
	return AccountUsage{
		ID:           account.ID,
		Provider:     string(account.Type),
		ProviderName: account.Type.DisplayName(),
		Email:        account.DisplayName(),
		Windows:      windows,
		UpdatedAt:    &now,
	}
}

func failedAccount(account auth.Account, message string) AccountUsage {
	now := time.Now()
	return AccountUsage{
		ID:           account.ID,
		Provider:     string(account.Type),
		ProviderName: account.Type.DisplayName(),
		Email:        account.DisplayName(),
		Error:        message,
		UpdatedAt:    &now,
	}
}

// authValues reads the auth file and returns its non-empty string fields.
func authValues(path string) (map[string]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var jsonData map[string]any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		return nil, false
	}
	values := make(map[string]string, len(jsonData))
	for key, value := range jsonData {
		if s, ok := value.(string); ok && s != "" {
			values[key] = s
		}
	}
	return values, true
}
