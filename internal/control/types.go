// Package control implements the daemon's local control API: HTTP/1.1 over a
// Unix socket, used by the Omarchy shell plugin (through `droidproxy ctl`),
// the CLI, and scripts. docs/control-api.md is the contract.
package control

import (
	"encoding/json"
)

// State is the full snapshot served by GET /v1/state and carried by "state"
// events. Every key is always present; slices encode as [] (never null).
type State struct {
	App       AppInfo         `json:"app"`
	Server    ServerState     `json:"server"`
	Settings  Settings        `json:"settings"`
	Paths     PathsInfo       `json:"paths"`
	Factory   FactoryState    `json:"factory"`
	Providers []ProviderState `json:"providers"`
	Copilot   CopilotState    `json:"copilot"`
	Meta      MetaState       `json:"meta"`
	Grok      GrokState       `json:"grok"`
	Usage     UsageState      `json:"usage"`
	Update    UpdateState     `json:"update"`
}

type AppInfo struct {
	Name               string `json:"name"`
	Version            string `json:"version"`
	Commit             string `json:"commit"`
	RepoURL            string `json:"repoUrl"`
	IssuesURL          string `json:"issuesUrl"`
	CLIProxyAPIURL     string `json:"cliProxyApiUrl"`
	CLIProxyAPIVersion string `json:"cliProxyApiVersion"`
}

type ServerState struct {
	Running      bool   `json:"running"`
	Starting     bool   `json:"starting"`
	ProxyPort    int    `json:"proxyPort"`
	BackendPort  int    `json:"backendPort"`
	URL          string `json:"url"`
	DashboardURL string `json:"dashboardUrl"`
	LastError    string `json:"lastError"`
}

type Settings struct {
	LaunchAtLogin             bool    `json:"launchAtLogin"`
	AllowRemote               bool    `json:"allowRemote"`
	SecretKey                 string  `json:"secretKey"`
	BindAddress               string  `json:"bindAddress"`
	Beta                      bool    `json:"beta"`
	VerboseLogging            bool    `json:"verboseLogging"`
	SequentialAccountFailover bool    `json:"sequentialAccountFailover"`
	OLEDTheme                 bool    `json:"oledTheme"`
	BackgroundOpacity         float64 `json:"backgroundOpacity"`
	GPT6AstraFastMode         bool    `json:"gpt6AstraFastMode"`
	GPT6SolFastMode           bool    `json:"gpt6SolFastMode"`
	GPT6LunaFastMode          bool    `json:"gpt6LunaFastMode"`
	MetaContributorMode       bool    `json:"metaContributorMode"`
	AutoCheckUpdates          bool    `json:"autoCheckUpdates"`
	AutoInstallUpdates        bool    `json:"autoInstallUpdates"`
}

type PathsInfo struct {
	AuthDir         string `json:"authDir"`
	LogsDir         string `json:"logsDir"`
	FactorySettings string `json:"factorySettings"`
	DebugLog        string `json:"debugLog"`
}

type FactoryState struct {
	ModelsInstalled bool `json:"modelsInstalled"`
}

// Provider kinds.
const (
	KindStandard = "standard"
	KindCopilot  = "copilot"
	KindMeta     = "meta"
	KindJunie    = "junie"
	KindGrok     = "grok"
)

type ProviderState struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Icon           string         `json:"icon"`
	Color          string         `json:"color"`
	Kind           string         `json:"kind"`
	Enabled        bool           `json:"enabled"`
	Authenticating bool           `json:"authenticating"`
	Help           string         `json:"help"`
	Accounts       []AccountState `json:"accounts"`
}

type AccountState struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Expired     bool   `json:"expired"`
	Disabled    bool   `json:"disabled"`
}

// Copilot gateway states.
const (
	GatewayIdle     = "idle"
	GatewayStarting = "starting"
	GatewayRunning  = "running"
	GatewayFailed   = "failed"
)

type CopilotState struct {
	Enabled         bool           `json:"enabled"`
	HasCredentials  bool           `json:"hasCredentials"`
	Authenticating  bool           `json:"authenticating"`
	DeviceCode      string         `json:"deviceCode"`
	VerificationURL string         `json:"verificationUrl"`
	GatewayState    string         `json:"gatewayState"`
	GatewayFailure  string         `json:"gatewayFailure"`
	Port            int            `json:"port"`
	AvailableModels []CopilotModel `json:"availableModels"`
	SelectedModels  []string       `json:"selectedModels"`
	MaxSelected     int            `json:"maxSelected"`
}

type CopilotModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MetaState struct {
	Authenticating  bool   `json:"authenticating"`
	DeviceCode      string `json:"deviceCode"`
	VerificationURL string `json:"verificationUrl"`
	LastError       string `json:"lastError"`
}

type GrokState struct {
	Authenticating  bool   `json:"authenticating"`
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
}

type UsageState struct {
	Visible    bool           `json:"visible"`
	Refreshing bool           `json:"refreshing"`
	Accounts   []UsageAccount `json:"accounts"`
}

type UsageAccount struct {
	Provider     string        `json:"provider"`
	ProviderName string        `json:"providerName"`
	Email        string        `json:"email"`
	Loading      bool          `json:"loading"`
	Error        string        `json:"error"`
	Windows      []UsageWindow `json:"windows"`
}

type UsageWindow struct {
	Title            string  `json:"title"`
	RemainingPercent float64 `json:"remainingPercent"`
	HasRemaining     bool    `json:"hasRemaining"`
	ResetText        string  `json:"resetText"`
}

// Update states.
const (
	UpdateIdle        = "idle"
	UpdateChecking    = "checking"
	UpdateUpToDate    = "upToDate"
	UpdateAvailable   = "available"
	UpdateDownloading = "downloading"
	UpdateInstalling  = "installing"
	UpdateError       = "error"
)

type UpdateState struct {
	State          string  `json:"state"`
	CurrentVersion string  `json:"currentVersion"`
	LatestVersion  string  `json:"latestVersion"`
	Notes          string  `json:"notes"`
	ReleaseURL     string  `json:"releaseUrl"`
	Error          string  `json:"error"`
	LastChecked    string  `json:"lastChecked"`
	Progress       float64 `json:"progress"`
}

// Normalized returns a copy of s whose slices (including nested ones) are
// non-nil, so they encode as [] instead of null. The receiver is not mutated,
// which keeps it safe to call on snapshots shared between goroutines.
func (s State) Normalized() State {
	if s.Providers == nil {
		s.Providers = []ProviderState{}
	} else {
		ps := make([]ProviderState, len(s.Providers))
		copy(ps, s.Providers)
		for i := range ps {
			if ps[i].Accounts == nil {
				ps[i].Accounts = []AccountState{}
			}
		}
		s.Providers = ps
	}
	if s.Copilot.AvailableModels == nil {
		s.Copilot.AvailableModels = []CopilotModel{}
	}
	if s.Copilot.SelectedModels == nil {
		s.Copilot.SelectedModels = []string{}
	}
	if s.Usage.Accounts == nil {
		s.Usage.Accounts = []UsageAccount{}
	} else {
		as := make([]UsageAccount, len(s.Usage.Accounts))
		copy(as, s.Usage.Accounts)
		for i := range as {
			if as[i].Windows == nil {
				as[i].Windows = []UsageWindow{}
			}
		}
		s.Usage.Accounts = as
	}
	if s.Update.State == "" {
		s.Update.State = UpdateIdle
	}
	return s
}

// MarshalJSON always encodes the normalized form.
func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	return json.Marshal(plain(s.Normalized()))
}

// Event types.
const (
	EventState   = "state"
	EventMessage = "message"
	EventOffline = "offline"
)

// Message levels.
const (
	LevelInfo  = "info"
	LevelError = "error"
)

// Event is one NDJSON line of GET /v1/events.
type Event struct {
	Type  string
	State *State
	// Message fields.
	ID    string
	Title string
	Body  string
	Level string
}

type stateEventJSON struct {
	Type  string `json:"type"`
	State State  `json:"state"`
}

type messageEventJSON struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Level string `json:"level"`
}

type bareEventJSON struct {
	Type string `json:"type"`
}

// MarshalJSON emits exactly the keys documented for each event type.
func (e Event) MarshalJSON() ([]byte, error) {
	switch e.Type {
	case EventState:
		var st State
		if e.State != nil {
			st = *e.State
		}
		return json.Marshal(stateEventJSON{Type: e.Type, State: st})
	case EventMessage:
		level := e.Level
		if level == "" {
			level = LevelInfo
		}
		return json.Marshal(messageEventJSON{Type: e.Type, ID: e.ID, Title: e.Title, Body: e.Body, Level: level})
	default:
		return json.Marshal(bareEventJSON{Type: e.Type})
	}
}

// UnmarshalJSON accepts any event line.
func (e *Event) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type  string `json:"type"`
		State *State `json:"state"`
		ID    string `json:"id"`
		Title string `json:"title"`
		Body  string `json:"body"`
		Level string `json:"level"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = Event{Type: raw.Type, State: raw.State, ID: raw.ID, Title: raw.Title, Body: raw.Body, Level: raw.Level}
	return nil
}

// DefaultTitle is used by clients when a CallResult has no title.
const DefaultTitle = "DroidProxy"

// CallResult is the body of every POST /v1/call/<method> response.
type CallResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	Title   string `json:"title,omitempty"`
}

// OK returns a successful result. An empty message means no dialog is needed.
func OK(message string) CallResult { return CallResult{OK: true, Message: message} }

// Fail returns a failed result carrying the user-facing error text.
func Fail(errText string) CallResult { return CallResult{OK: false, Error: errText} }

// WithTitle returns r with its dialog title set.
func (r CallResult) WithTitle(title string) CallResult {
	r.Title = title
	return r
}
