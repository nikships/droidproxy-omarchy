package prefs

import (
	"fmt"
	"sync"
	"testing"
)

// Concurrent per-model toggles must not lose each other's updates: the
// map is read-modify-written as a whole, so without one lock across the
// operation the last writer silently restores models another call
// disabled (missing entries read as enabled).
func TestSetModelEnabledConcurrent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := SetModelEnabled(fmt.Sprintf("model-%d", i), false); err != nil {
				t.Errorf("SetModelEnabled: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got := EnabledModels()
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("model-%d", i)
		if v, ok := got[slug]; !ok || v {
			t.Fatalf("enabledModels[%s] = %v, %v; concurrent update lost (map has %d entries)", slug, v, ok, len(got))
		}
	}
}

func TestSetProviderEnabledConcurrent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := SetProviderEnabled(fmt.Sprintf("provider-%d", i), false); err != nil {
				t.Errorf("SetProviderEnabled: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got := EnabledProviders()
	for i := 0; i < n; i++ {
		provider := fmt.Sprintf("provider-%d", i)
		if v, ok := got[provider]; !ok || v {
			t.Fatalf("enabledProviders[%s] = %v, %v; concurrent update lost (map has %d entries)", provider, v, ok, len(got))
		}
	}
}
