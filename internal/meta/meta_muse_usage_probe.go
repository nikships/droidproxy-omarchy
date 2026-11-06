package meta

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

// usageProbeInterval is how old a stored snapshot must be before the probe
// spends quota on a fresh one. Real traffic or an earlier probe this recent
// already counts as fresh, which also keeps polling agents from burning quota
// on probes.
const usageProbeInterval = 60 * time.Second

const usageProbeBody = `{"model":"muse-wonder-1.0","input":"hi","max_output_tokens":16,"stream":true}`

var usageProbeClient = &http.Client{Timeout: 20 * time.Second}

// isUsageFresh reports whether snapshot was observed within usageProbeInterval.
func isUsageFresh(snapshot *UsageSnapshot, now time.Time) bool {
	return snapshot != nil && now.Sub(snapshot.ObservedAt) < usageProbeInterval
}

// UsageProber refreshes stale Meta usage (macOS MetaMuseUsageProbe). Meta has
// no usage endpoint, and the quota only rides along on streamed /v1/responses
// traffic, so a snapshot sniffed from real requests goes stale whenever Meta
// is idle. The probe asks for a 16-token streamed completion with the
// account's own key and records the `response.subscription_usage` event, the
// same data ThinkingProxy would have sniffed, then drops the stream.
type UsageProber struct {
	Credentials *CredentialStore
	Usage       *UsageStore
	BaseURL     string
	Client      *http.Client
}

// NewUsageProber probes api.meta.ai with the shared credential and usage stores.
func NewUsageProber() *UsageProber {
	return &UsageProber{
		Credentials: Shared(),
		Usage:       SharedUsageStore(),
		BaseURL:     "https://" + APIHost,
		Client:      usageProbeClient,
	}
}

// Refresh records a fresh snapshot for the account when its stored one is
// older than usageProbeInterval (or always, with force). Any failure keeps
// the stored snapshot, so the card falls back to its "as of" value. It
// reports whether a request completed and recorded a snapshot.
func (p *UsageProber) Refresh(accountID string, force bool) bool {
	now := time.Now()
	if !force && isUsageFresh(p.Usage.Snapshot(accountID), now) {
		return false
	}
	var apiKey string
	for _, a := range p.Credentials.Accounts() {
		if a.ID == accountID && !a.Disabled && a.Credentials.APIKey != "" &&
			!APIKeyExpiredAt(a.Credentials.APIKeyExpiresAt, now) {
			apiKey = a.Credentials.APIKey
			break
		}
	}
	if apiKey == "" {
		return false
	}

	request, err := http.NewRequest(http.MethodPost, p.BaseURL+"/v1/responses", strings.NewReader(usageProbeBody))
	if err != nil {
		logx.Logf("[Meta] Usage probe failed: %v", err)
		return false
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", "muse-build/1.3.0")

	response, err := p.Client.Do(request)
	if err != nil {
		logx.Logf("[Meta] Usage probe failed: %v", err)
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		logx.Logf("[Meta] Usage probe got HTTP %d", response.StatusCode)
		return false
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") && strings.Contains(line, "response.subscription_usage") {
			snapshot := ParseUsageEvent(line, time.Now())
			if snapshot == nil {
				return false
			}
			p.Usage.Record(accountID, *snapshot)
			return true
		}
		if readErr != nil {
			if readErr != io.EOF {
				logx.Logf("[Meta] Usage probe failed: %v", readErr)
			}
			return false
		}
	}
}
