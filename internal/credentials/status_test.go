package credentials

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

func unsignedToken(subject string, generation, expires int64) string {
	payload := fmt.Sprintf(`{"sub":%q,"exp":%d,"gen":%d}`, subject, expires, generation)
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestValidateJWTUsesClankClaims(t *testing.T) {
	expires := time.Now().Add(90 * 24 * time.Hour).Unix()
	token := unsignedToken("server:abc", 4, expires)
	if err := ValidateJWT(token, "abc", 4, expires); err != nil {
		t.Fatal(err)
	}
	if err := ValidateJWT(token, "other", 4, expires); err == nil {
		t.Fatal("expected identity mismatch")
	}
	if err := ValidateJWT(token, "abc", 5, expires); err == nil {
		t.Fatal("expected generation mismatch")
	}
}

func TestTokenExpiryIsModeAware(t *testing.T) {
	expires := time.Now().Add(10 * 24 * time.Hour).Unix()
	got, err := Expiry("token", unsignedToken("server:abc", 1, expires), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Unix() != expires {
		t.Fatalf("got %d want %d", got.Unix(), expires)
	}
}
