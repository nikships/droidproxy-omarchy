package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/meta"
)

// knownClaudeBuckets are the Claude usage buckets we surface, in display
// order. Unknown buckets are ignored.
var knownClaudeBuckets = []struct{ key, title string }{
	{"five_hour", "5-hour"},
	{"seven_day", "Weekly"},
	{"seven_day_opus", "Weekly (Opus)"},
	{"seven_day_sonnet", "Weekly (Sonnet)"},
	{"seven_day_oauth_apps", "Weekly (OAuth Apps)"},
	{"seven_day_cowork", "Weekly (Cowork)"},
	{"seven_day_omelette", "Weekly (Omelette)"},
}

// ParseClaudeWindows parses the Claude OAuth usage payload. The API defines
// "utilization" as a percentage in [0, 100] (1.0 is 1%, not 100%); values are
// clamped defensively.
func ParseClaudeWindows(data []byte) []Window {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	var windows []Window
	for _, bucket := range knownClaudeBuckets {
		dict, ok := raw[bucket.key].(map[string]any)
		if !ok {
			continue
		}
		utilization, ok := numberValue(dict["utilization"])
		if !ok {
			continue
		}
		usedPercent := utilization
		if usedPercent < 0 {
			usedPercent = 0
		}
		if usedPercent > 100 {
			usedPercent = 100
		}
		resetsAt, _ := dict["resets_at"].(string)
		resetDate := parseISO8601Date(resetsAt)
		resetText := resetsAt
		if resetDate != nil {
			resetText = ResetTextFor(*resetDate)
		}
		windows = append(windows, newWindow(bucket.title, &usedPercent, resetText, resetDate))
	}
	return windows
}

// ParseGrokWindows parses the SuperGrok pooled credit window
// (cli-chat-proxy.grok.com/v1/billing?format=credits). SuperGrok reports one
// window per billing period: creditUsagePercent when present, otherwise
// onDemandUsed/onDemandCap. The title comes from currentPeriod.type.
func ParseGrokWindows(data []byte) []Window {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	config, ok := raw["config"].(map[string]any)
	if !ok {
		return nil
	}

	var usedPercent float64
	if credit, ok := numberValue(config["creditUsagePercent"]); ok {
		usedPercent = credit
	} else if capDict, ok := config["onDemandCap"].(map[string]any); ok {
		cap, capOK := numberValue(capDict["val"])
		usedDict, _ := config["onDemandUsed"].(map[string]any)
		used, usedOK := numberValue(usedDict["val"])
		if !capOK || !usedOK || cap <= 0 {
			return nil
		}
		usedPercent = used / cap * 100
	} else {
		return nil
	}
	if usedPercent < 0 {
		usedPercent = 0
	}
	if usedPercent > 100 {
		usedPercent = 100
	}

	period, _ := config["currentPeriod"].(map[string]any)
	title := "Credits"
	if period != nil {
		switch period["type"] {
		case "USAGE_PERIOD_TYPE_WEEKLY":
			title = "Weekly"
		case "USAGE_PERIOD_TYPE_MONTHLY":
			title = "Monthly"
		}
	}

	var resetString string
	if period != nil {
		resetString, _ = period["end"].(string)
	}
	if resetString == "" {
		resetString, _ = config["billingPeriodEnd"].(string)
	}
	resetDate := parseISO8601Date(resetString)
	resetText := resetString
	if resetDate != nil {
		resetText = ResetTextFor(*resetDate)
	}
	return []Window{newWindow(title, &usedPercent, resetText, resetDate)}
}

// ParseMetaWindows renders a last-observed Meta usage snapshot as the 5-hour
// window plus the weekly window. Both reset texts carry the "as of" stamp,
// matching `muse /usage` semantics.
func ParseMetaWindows(snapshot meta.UsageSnapshot, now time.Time) []Window {
	observed := relativeTimeString(snapshot.ObservedAt, now)
	shortTitle := MetaWindowTitle(snapshot.WindowDurationMins)
	shortReset := ResetTextForAt(snapshot.WindowResetsAt, now) + " · as of " + observed
	weeklyReset := ResetTextForAt(snapshot.WeeklyResetsAt, now) + " · as of " + observed
	shortUsed := snapshot.WindowUsedPercent
	weeklyUsed := snapshot.WeeklyUsedPercent
	return []Window{
		newWindow(shortTitle, &shortUsed, shortReset, &snapshot.WindowResetsAt),
		newWindow("Weekly", &weeklyUsed, weeklyReset, &snapshot.WeeklyResetsAt),
	}
}

// MetaWindowTitle names the short Meta window from its duration in minutes
// ("5-hour" for the standard 300-minute window).
func MetaWindowTitle(minutes float64) string {
	if minutes <= 0 {
		return "Window"
	}
	if math.Mod(minutes, 60) == 0 {
		return fmt.Sprintf("%d-hour", int(minutes/60))
	}
	return fmt.Sprintf("%d-min", int(math.Round(minutes)))
}

// parseCodexWindows parses the Codex backend usage payload, falling back to a
// generic dictionary walk for unknown shapes.
func parseCodexWindows(root any) []Window {
	rateLimit, ok := root.(map[string]any)["rate_limit"].(map[string]any)
	if !ok {
		return parseGenericWindows(root)
	}

	var windows []Window
	if w := codexWindow("5-hour", rateLimit["primary_window"]); w != nil {
		windows = append(windows, *w)
	}
	if w := codexWindow("Weekly", rateLimit["secondary_window"]); w != nil {
		windows = append(windows, *w)
	}
	return windows
}

func codexWindow(title string, value any) *Window {
	window, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	resetDate := resetDateFromDict(window)
	usedPercent, hasUsed := numberValue(window["used_percent"])
	w := Window{Title: title, ResetDate: resetDate}
	if hasUsed {
		w.UsedPercent = &usedPercent
		remaining := 100 - usedPercent
		if remaining < 0 {
			remaining = 0
		}
		w.RemainingPercent = &remaining
		w.HasRemaining = remaining > 0
	}
	if resetDate != nil {
		w.ResetText = ResetTextFor(*resetDate)
	} else {
		w.ResetText = resetTextFromDict(window)
	}
	return &w
}

// parseGenericWindows handles undocumented Codex-shaped responses by walking
// every nested dictionary and pattern-matching on keys and values.
func parseGenericWindows(root any) []Window {
	var windows []Window
	for _, dict := range flattenDictionaries(root) {
		title := windowTitleFromDict(dict)
		if title == "" {
			continue
		}
		duplicate := false
		for _, w := range windows {
			if w.Title == title {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		percent := percentValueFromDict(dict)
		reset := resetTextFromDict(dict)
		if percent != nil || reset != "" {
			windows = append(windows, newWindow(title, percent, reset, nil))
		}
	}
	sort.SliceStable(windows, func(i, j int) bool {
		return windowRank(windows[i].Title) < windowRank(windows[j].Title)
	})
	return windows
}

func flattenDictionaries(value any) []map[string]any {
	if dict, ok := value.(map[string]any); ok {
		out := []map[string]any{dict}
		for _, v := range dict {
			out = append(out, flattenDictionaries(v)...)
		}
		return out
	}
	if list, ok := value.([]any); ok {
		var out []map[string]any
		for _, v := range list {
			out = append(out, flattenDictionaries(v)...)
		}
		return out
	}
	return nil
}

func windowTitleFromDict(dict map[string]any) string {
	var parts []string
	for key, value := range dict {
		parts = append(parts, fmt.Sprintf("%s:%v", key, value))
	}
	// Map iteration is random; sort so detection is deterministic.
	sort.Strings(parts)
	joined := strings.ToLower(strings.Join(parts, " "))
	switch {
	case strings.Contains(joined, "5h") || strings.Contains(joined, "5-hour") || strings.Contains(joined, "five"):
		return "5-hour"
	case strings.Contains(joined, "weekly") || strings.Contains(joined, "week") || strings.Contains(joined, "7d"):
		return "Weekly"
	case strings.Contains(joined, "full") || strings.Contains(joined, "premium") || strings.Contains(joined, "paid"):
		return "Full"
	case strings.Contains(joined, "standard") || strings.Contains(joined, "session"):
		return "Session"
	}
	return ""
}

func percentValueFromDict(dict map[string]any) *float64 {
	for _, keys := range [][]string{{"percent", "percentage", "%"}, {"usage", "used", "fraction"}} {
		for key, value := range dict {
			lower := strings.ToLower(key)
			matched := false
			for _, k := range keys {
				if strings.Contains(lower, k) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			number, ok := numberValue(value)
			if !ok {
				continue
			}
			if number <= 1 {
				number *= 100
			}
			return &number
		}
	}
	return nil
}

func resetTextFromDict(dict map[string]any) string {
	if date := resetDateFromDict(dict); date != nil {
		return ResetTextFor(*date)
	}
	for key, value := range dict {
		if !strings.Contains(strings.ToLower(key), "reset") {
			continue
		}
		if s, ok := value.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func resetDateFromDict(dict map[string]any) *time.Time {
	for key, value := range dict {
		lowerKey := strings.ToLower(key)
		if !strings.Contains(lowerKey, "reset") {
			continue
		}
		if s, ok := value.(string); ok && s != "" {
			if date := parseISO8601Date(s); date != nil {
				return date
			}
			continue
		}
		if number, ok := numberValue(value); ok {
			now := time.Now()
			if strings.Contains(lowerKey, "after") {
				d := now.Add(time.Duration(number * float64(time.Second)))
				return &d
			}
			seconds := number
			if number > 10_000_000_000 {
				seconds = number / 1000
			}
			d := time.Unix(int64(seconds), 0)
			return &d
		}
	}
	return nil
}

func parseISO8601Date(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// ResetTextFor formats a reset date like the Swift tracker's
// RelativeDateTimeFormatter + medium-date/short-time pair, e.g.
// "in 2 hours (Jun 5, 2026 at 1:00 PM)".
func ResetTextFor(date time.Time) string {
	return ResetTextForAt(date, time.Now())
}

// ResetTextForAt is ResetTextFor with an explicit "now" (tests and the Meta
// "as of" rendering).
func ResetTextForAt(date, now time.Time) string {
	return relativeTimeString(date, now) + " (" + date.Format("Jan 2, 2006 at 3:04 PM") + ")"
}

func relativeTimeString(date, now time.Time) string {
	d := date.Sub(now)
	prefix, suffix := "in ", ""
	if d < 0 {
		prefix, suffix = "", " ago"
		d = -d
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds())
	switch {
	case minutes >= 60*24*365:
		return prefix + plural(minutes/(60*24*365), "year") + suffix
	case minutes >= 60*24*30:
		return prefix + plural(minutes/(60*24*30), "month") + suffix
	case minutes >= 60*24*7:
		return prefix + plural(minutes/(60*24*7), "week") + suffix
	case minutes >= 60*24:
		return prefix + plural(minutes/(60*24), "day") + suffix
	case minutes >= 60:
		return prefix + plural(minutes/60, "hour") + suffix
	case minutes >= 1:
		return prefix + plural(minutes, "minute") + suffix
	default:
		return prefix + plural(seconds, "second") + suffix
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := parsePercentString(v)
		return f, err == nil
	}
	return 0, false
}

func parsePercentString(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(strings.ReplaceAll(s, "%", ""), "%g", &f)
	return f, err
}

func windowRank(title string) int {
	switch title {
	case "5-hour":
		return 0
	case "Session":
		return 1
	case "Weekly":
		return 2
	case "Full":
		return 3
	default:
		return 9
	}
}
