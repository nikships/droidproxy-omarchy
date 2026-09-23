package updater

import (
	"strconv"
	"strings"
)

// CompareVersions compares two dotted version strings, ignoring a leading
// "v". A version with a pre-release suffix (1.2.3-beta) sorts before the
// same release (1.2.3). Non-numeric parts compare as 0, so garbage sorts
// lowest and never triggers an install over a real version.
func CompareVersions(a, b string) int {
	na, pa := parseVersion(a)
	nb, pb := parseVersion(b)
	if c := compareParts(na, nb); c != 0 {
		return c
	}
	// Equal release numbers: no pre-release > pre-release.
	switch {
	case pa == "" && pb == "":
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	return strings.Compare(pa, pb)
}

// parseVersion returns the numeric parts (up to 4) and the pre-release suffix.
func parseVersion(v string) ([4]int, string) {
	var parts [4]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		if v[i] == '-' {
			// preserve suffix; ignore build metadata after '+'
			return fillParts(parts, v[:i]), v[i+1:]
		}
		return fillParts(parts, v[:i]), ""
	}
	return fillParts(parts, v), ""
}

func fillParts(parts [4]int, core string) [4]int {
	for i, s := range strings.SplitN(core, ".", 4) {
		if i > 3 {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			n = 0
		}
		if n < 0 {
			n = 0
		}
		parts[i] = n
	}
	return parts
}

func compareParts(a, b [4]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
