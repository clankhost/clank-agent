// Package credentials provides mode-aware agent control credential inspection.
package credentials

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/clankhost/clank-agent/internal/certs"
)

const RenewalWindow = 30 * 24 * time.Hour

type JWTClaims struct {
	Subject        string `json:"sub"`
	ExpiresAt      int64  `json:"exp"`
	AuthGeneration int64  `json:"gen"`
}

// ParseJWTClaims reads non-secret identity/expiry claims for local validation.
// Signature validation still occurs at the control plane on every connection.
func ParseJWTClaims(token string) (JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return JWTClaims{}, fmt.Errorf("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return JWTClaims{}, fmt.Errorf("decoding JWT claims: %w", err)
	}
	var claims JWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return JWTClaims{}, fmt.Errorf("parsing JWT claims: %w", err)
	}
	if claims.Subject == "" || claims.ExpiresAt == 0 {
		return JWTClaims{}, fmt.Errorf("JWT is missing required claims")
	}
	return claims, nil
}

// Expiry returns the credential actually used by the configured auth mode.
func Expiry(authMode, authToken string, store *certs.Store, configuredUnix int64) (time.Time, error) {
	if authMode == "token" {
		claims, err := ParseJWTClaims(authToken)
		if err != nil {
			if configuredUnix > 0 {
				return time.Unix(configuredUnix, 0), nil
			}
			return time.Time{}, err
		}
		return time.Unix(claims.ExpiresAt, 0), nil
	}
	certPath, _, _ := store.ActivePaths()
	data, err := os.ReadFile(certPath)
	if err != nil {
		if configuredUnix > 0 {
			return time.Unix(configuredUnix, 0), nil
		}
		return time.Time{}, fmt.Errorf("reading active certificate: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, fmt.Errorf("parsing active certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing active certificate: %w", err)
	}
	return cert.NotAfter, nil
}

func ValidateJWT(token, serverID string, authGeneration, expiresUnix int64) error {
	claims, err := ParseJWTClaims(token)
	if err != nil {
		return err
	}
	if claims.Subject != "server:"+serverID {
		return fmt.Errorf("renewed JWT has wrong server identity")
	}
	if claims.AuthGeneration != authGeneration {
		return fmt.Errorf("renewed JWT has wrong auth generation")
	}
	if claims.ExpiresAt != expiresUnix {
		return fmt.Errorf("renewed JWT has inconsistent expiry")
	}
	if time.Until(time.Unix(claims.ExpiresAt, 0)) < 24*time.Hour {
		return fmt.Errorf("renewed JWT expires too soon")
	}
	return nil
}
