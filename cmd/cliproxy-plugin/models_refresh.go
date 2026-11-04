package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// modelRefreshInterval is how often the plugin reloads Work's model list on
// its own. Work retires and adds models without notice, and CPA only asks the
// plugin for models when the account is (re)loaded.
const modelRefreshInterval = time.Hour

// refreshEvery reads DOUBAO_MODEL_REFRESH_INTERVAL (a Go duration, test use)
// and falls back to modelRefreshInterval.
func refreshEvery() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("DOUBAO_MODEL_REFRESH_INTERVAL")); err == nil && d >= time.Second {
		return d
	}
	return modelRefreshInterval
}

// watchModels refreshes the model list of the open service until ctx ends and
// tells CPA when the list changed, so that retired models leave /v1/models.
func watchModels(ctx context.Context) {
	ticker := time.NewTicker(refreshEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		refreshModelsOnce(ctx)
	}
}

func refreshModelsOnce(ctx context.Context) {
	runtime.Lock()
	s := runtime.service
	runtime.Unlock()
	// Never open the service from here: loading it logs in and starts the
	// task gateway, which CPA does itself when it needs the account.
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	added, removed, err := s.RefreshModels(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "GATEWAY_MODELS_REFRESH_FAILED error=%q\n", err.Error())
		return
	}
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	notified := "true"
	if err := notifyModelChange(""); err != nil {
		notified = "false error=" + fmt.Sprintf("%q", err.Error())
	}
	fmt.Fprintf(os.Stderr, "GATEWAY_MODELS_CHANGED added=%s removed=%s cpa_notified=%s\n", strings.Join(added, ","), strings.Join(removed, ","), notified)
}
