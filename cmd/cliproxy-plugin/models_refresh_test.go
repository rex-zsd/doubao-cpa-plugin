package main

import (
	"context"
	"testing"
	"time"
)

func TestRefreshEvery(t *testing.T) {
	for value, want := range map[string]time.Duration{"": modelRefreshInterval, "junk": modelRefreshInterval, "10ms": modelRefreshInterval, "90s": 90 * time.Second} {
		t.Setenv("DOUBAO_MODEL_REFRESH_INTERVAL", value)
		if got := refreshEvery(); got != want {
			t.Fatalf("%q: got %v, want %v", value, got, want)
		}
	}
}

// Without an open service the watcher must not load one on its own.
func TestRefreshModelsOnceSkipsClosedService(t *testing.T) {
	runtime.Lock()
	saved := runtime.service
	runtime.service = nil
	runtime.Unlock()
	defer func() {
		runtime.Lock()
		runtime.service = saved
		runtime.Unlock()
	}()
	refreshModelsOnce(context.Background())
	runtime.Lock()
	defer runtime.Unlock()
	if runtime.service != nil {
		t.Fatal("watcher opened the service")
	}
}
