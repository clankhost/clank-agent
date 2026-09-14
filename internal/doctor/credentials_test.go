package doctor

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func testJWT(expiry int64) string {
	payload := fmt.Sprintf(`{"sub":"server:test","exp":%d,"gen":1}`, expiry)
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestCheckCredentialsUsesJWTExpiryInTunnelMode(t *testing.T) {
	result := CheckCredentials(
		t.TempDir(),
		"token",
		testJWT(time.Now().Add(7*24*time.Hour).Unix()),
		0,
		"failed",
		"recovery_failed",
	)
	if result.Status != Error {
		t.Fatalf("got %v, want error: %s", result.Status, result.Message)
	}
}

func TestCheckCredentialsReportsHealthyLongLivedJWTWithoutCertificate(t *testing.T) {
	result := CheckCredentials(
		t.TempDir(),
		"token",
		testJWT(time.Now().Add(60*24*time.Hour).Unix()),
		0,
		"healthy",
		"",
	)
	if result.Status != OK {
		t.Fatalf("got %v, want OK: %s", result.Status, result.Message)
	}
}
