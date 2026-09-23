package claude

import "strings"

// containsSubstring mirrors the Swift tests' `sanitized.contains(...)`.
func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
