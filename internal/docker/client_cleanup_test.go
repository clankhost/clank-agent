package docker

import (
	"context"
	"strings"
	"testing"
)

func TestLegacyApplyCleanupIsDisabled(t *testing.T) {
	manager := &Manager{}

	summary, err := manager.ApplyCleanup(
		context.Background(),
		[]string{"registry.example.com/service:protected"},
	)

	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "disabled") {
		t.Fatalf("expected disabled cleanup error, got %v", err)
	}
	if summary != nil {
		t.Fatalf("expected no cleanup summary, got %#v", summary)
	}
}
