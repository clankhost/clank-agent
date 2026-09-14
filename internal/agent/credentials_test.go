package agent

import (
	"testing"
	"time"
)

func TestRenewalBackoffIsExponentialAndCapped(t *testing.T) {
	if got := renewalBackoff(1); got != time.Minute {
		t.Fatalf("attempt 1: %s", got)
	}
	if got := renewalBackoff(2); got != 2*time.Minute {
		t.Fatalf("attempt 2: %s", got)
	}
	if got := renewalBackoff(5); got != 16*time.Minute {
		t.Fatalf("attempt 5: %s", got)
	}
	if got := renewalBackoff(99); got != 24*time.Hour {
		t.Fatalf("cap: %s", got)
	}
}

func TestDefaultRenewalEndpointSupportsTunnelAndDirectEndpoints(t *testing.T) {
	cases := map[string]string{
		"grpc.clank.host:443": "https://clank.host/api/agent/renew",
		"clank.example:50052": "https://clank.example/api/agent/renew",
	}
	for input, want := range cases {
		if got := defaultRenewalEndpoint(input); got != want {
			t.Fatalf("%s: got %s want %s", input, got, want)
		}
	}
}

func TestSafeCodeCannotPersistSecretText(t *testing.T) {
	if got := safeCode("write failed: token=secret/value"); got != "unknown" {
		t.Fatalf("unexpected safe code %q", got)
	}
}
