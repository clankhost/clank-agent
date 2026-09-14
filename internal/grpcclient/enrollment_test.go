package grpcclient

import (
	"context"
	"strings"
	"testing"
)

func TestRenewCredentialsRefusesPlaintextEndpoint(t *testing.T) {
	_, err := RenewCredentials(
		context.Background(),
		"http://control.example/api/agent/renew",
		"server-id",
		"renewal-secret",
		"rotation-id",
		"mtls",
		[]byte("csr"),
	)
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("got %v, want HTTPS validation error", err)
	}
}
