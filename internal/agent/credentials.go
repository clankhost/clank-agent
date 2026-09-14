package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/certs"
	"github.com/clankhost/clank-agent/internal/credentials"
	"github.com/clankhost/clank-agent/internal/grpcclient"
)

const renewalResponseRetry = 5 * time.Minute

func (a *Agent) credentialSnapshot() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return *a.cfg
}

func (a *Agent) saveCredentialConfig(cfg Config) error {
	if err := SaveConfig(a.cfgDir, &cfg); err != nil {
		return err
	}
	copyCredentialFields(a.cfg, &cfg)
	return nil
}

func copyCredentialFields(dst, src *Config) {
	dst.AuthToken = src.AuthToken
	dst.RenewalToken = src.RenewalToken
	dst.RenewalEndpoint = src.RenewalEndpoint
	dst.CredentialExpiresUnix = src.CredentialExpiresUnix
	dst.AuthGeneration = src.AuthGeneration
	dst.RenewalStatus = src.RenewalStatus
	dst.RenewalAttempts = src.RenewalAttempts
	dst.RenewalLastError = src.RenewalLastError
	dst.RenewalLastSuccessUnix = src.RenewalLastSuccessUnix
	dst.RenewalNextRetryUnix = src.RenewalNextRetryUnix
	dst.PendingRotationID = src.PendingRotationID
	dst.PendingAuthToken = src.PendingAuthToken
	dst.PendingRenewalToken = src.PendingRenewalToken
	dst.PendingExpiresUnix = src.PendingExpiresUnix
	dst.PendingAuthGeneration = src.PendingAuthGeneration
	dst.PendingPreviousBundle = src.PendingPreviousBundle
	dst.PendingHadPointer = src.PendingHadPointer
}

func newRotationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rotation-" + hex.EncodeToString(b), nil
}

func (a *Agent) beginCredentialRequest(now time.Time) (Config, []byte, error) {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()

	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	cfg := *a.cfg
	if cfg.PendingRotationID == "" {
		requestID, err := newRotationID()
		if err != nil {
			return Config{}, nil, err
		}
		cfg.PendingRotationID = requestID
	}
	csr, err := a.certStore.PrepareRotation(cfg.PendingRotationID, cfg.ServerID)
	if err != nil {
		return Config{}, nil, err
	}
	cfg.RenewalStatus = "pending"
	cfg.RenewalAttempts++
	cfg.RenewalLastError = ""
	cfg.RenewalNextRetryUnix = now.Add(renewalResponseRetry).Unix()
	if err := a.saveCredentialConfig(cfg); err != nil {
		return Config{}, nil, fmt.Errorf("saving pending renewal state: %w", err)
	}
	return cfg, csr, nil
}

func (a *Agent) maybeRequestCredentialRenewal(stream grpcclient.ConnectStream) error {
	now := time.Now()
	cfg := a.credentialSnapshot()
	expires, err := credentials.Expiry(cfg.AuthMode, cfg.AuthToken, a.certStore, cfg.CredentialExpiresUnix)
	if err != nil {
		a.recordCredentialFailure("expiry_unavailable", false)
		return nil
	}
	if !credentialRenewalDue(cfg, expires, now) {
		return nil
	}
	if cfg.RenewalNextRetryUnix > now.Unix() {
		return nil
	}
	cfg, csr, err := a.beginCredentialRequest(now)
	if err != nil {
		a.recordCredentialFailure("prepare_failed", true)
		return nil
	}
	log.Printf("[credentials] Requesting automatic renewal request_id=%s expires_at=%s", cfg.PendingRotationID, expires.UTC().Format(time.RFC3339))
	if err := stream.Send(&clankv1.AgentMessage{Payload: &clankv1.AgentMessage_CredentialRenewalRequest{
		CredentialRenewalRequest: &clankv1.CredentialRenewalRequest{
			RequestId: cfg.PendingRotationID, CsrPem: csr,
			CurrentAuthGeneration: cfg.AuthGeneration, AuthMode: normalizedAuthMode(cfg.AuthMode),
		},
	}}); err != nil {
		a.recordCredentialFailure("send_failed", false)
		return fmt.Errorf("sending credential renewal request: %w", err)
	}
	return nil
}

func credentialRenewalDue(cfg Config, expires, now time.Time) bool {
	// Agents enrolled before renewable credentials were introduced have no
	// recovery token. Bootstrap one immediately while their existing control
	// channel is authenticated, rather than waiting until the normal renewal
	// window and risking an unrecoverable offline expiry.
	return cfg.PendingRotationID != "" || cfg.RenewalToken == "" || expires.Sub(now) <= credentials.RenewalWindow
}

func (a *Agent) handleCredentialRotation(
	ctx context.Context,
	stream grpcclient.ConnectStream,
	rotation *clankv1.CredentialRotation,
) bool {
	if rotation.GetErrorCode() != "" {
		log.Printf("[credentials] Renewal rejected request_id=%s code=%s", rotation.GetRequestId(), safeCode(rotation.GetErrorCode()))
		a.recordCredentialFailure(rotation.GetErrorCode(), true)
		return false
	}
	if err := a.applyCredentialRotation(rotation); err != nil {
		log.Printf("[credentials] Credential cutover failed request_id=%s: %v", rotation.GetRequestId(), err)
		// Keep the request-scoped key/CSR so a lost failure acknowledgement
		// can retry the server's idempotent staged bundle after backoff.
		a.recordCredentialFailure("activation_failed", false)
		_ = stream.Send(rotationResult(rotation.GetRequestId(), false, "activation", "activation_failed"))
		return false
	}
	if err := stream.Send(rotationResult(rotation.GetRequestId(), true, "activated", "")); err != nil {
		log.Printf("[credentials] New credentials activated; acknowledgement will follow reconnect")
	}
	log.Printf("[credentials] Credential renewal activated request_id=%s; reconnecting", rotation.GetRequestId())
	return true
}

func rotationResult(requestID string, success bool, stage, code string) *clankv1.AgentMessage {
	return &clankv1.AgentMessage{Payload: &clankv1.AgentMessage_CredentialRotationResult{
		CredentialRotationResult: &clankv1.CredentialRotationResult{
			RequestId: requestID, Success: success, Stage: stage, ErrorCode: code,
		},
	}}
}

func (a *Agent) applyCredentialRotation(rotation *clankv1.CredentialRotation) error {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()

	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	old := *a.cfg
	if rotation.GetRequestId() == "" || rotation.GetRequestId() != old.PendingRotationID {
		return fmt.Errorf("rotation response does not match pending request")
	}
	if len(rotation.GetClientCert()) == 0 || len(rotation.GetCaCert()) == 0 || rotation.GetRenewalToken() == "" || rotation.GetExpiresUnix() == 0 {
		return fmt.Errorf("rotation response is incomplete")
	}
	if normalizedAuthMode(old.AuthMode) == "token" {
		if err := credentials.ValidateJWT(rotation.GetAuthToken(), old.ServerID, rotation.GetAuthGeneration(), rotation.GetExpiresUnix()); err != nil {
			return err
		}
	}
	if err := a.certStore.StageRotation(rotation.GetRequestId(), old.ServerID, rotation.GetClientCert(), rotation.GetCaCert()); err != nil {
		return err
	}

	pending := old
	pending.PendingAuthToken = rotation.GetAuthToken()
	pending.PendingRenewalToken = rotation.GetRenewalToken()
	pending.PendingExpiresUnix = rotation.GetExpiresUnix()
	pending.PendingAuthGeneration = rotation.GetAuthGeneration()
	pending.PendingPreviousBundle, pending.PendingHadPointer = a.certStore.CurrentPointer()
	if err := a.saveCredentialConfig(pending); err != nil {
		_ = a.certStore.DiscardRotation(rotation.GetRequestId())
		return fmt.Errorf("persisting pending credential metadata: %w", err)
	}

	activation, err := a.certStore.ActivateRotation(rotation.GetRequestId())
	if err != nil {
		if rollbackErr := a.saveCredentialConfig(old); rollbackErr == nil {
			_ = a.certStore.DiscardRotation(rotation.GetRequestId())
		} else {
			return fmt.Errorf("activating credential bundle: %w (config rollback failed: %v)", err, rollbackErr)
		}
		return err
	}
	final := pending
	final.AuthToken = pending.PendingAuthToken
	final.RenewalToken = pending.PendingRenewalToken
	if final.RenewalEndpoint == "" {
		final.RenewalEndpoint = defaultRenewalEndpoint(final.GRPCEndpoint)
	}
	final.CredentialExpiresUnix = pending.PendingExpiresUnix
	final.AuthGeneration = pending.PendingAuthGeneration
	final.RenewalStatus = "healthy"
	final.RenewalAttempts = 0
	final.RenewalLastError = ""
	final.RenewalLastSuccessUnix = time.Now().Unix()
	final.RenewalNextRetryUnix = 0
	clearPendingCredentialFields(&final)
	if err := a.saveCredentialConfig(final); err != nil {
		pointerRollbackErr := a.certStore.Rollback(activation)
		configRollbackErr := a.saveCredentialConfig(old)
		if configRollbackErr == nil {
			_ = a.certStore.DiscardRotation(rotation.GetRequestId())
		}
		if pointerRollbackErr != nil || configRollbackErr != nil {
			return fmt.Errorf(
				"committing credential metadata: %w (pointer rollback: %v, config rollback: %v)",
				err,
				pointerRollbackErr,
				configRollbackErr,
			)
		}
		return fmt.Errorf("committing credential metadata: %w", err)
	}
	return nil
}

// resumePendingCredentialCutover completes a crash-interrupted activation.
func (a *Agent) resumePendingCredentialCutover() error {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	cfg := *a.cfg
	if cfg.PendingRotationID == "" || cfg.PendingRenewalToken == "" || cfg.PendingExpiresUnix == 0 {
		return nil
	}
	_, err := a.certStore.ActivateRotation(cfg.PendingRotationID)
	if err != nil {
		return fmt.Errorf("resuming credential activation: %w", err)
	}
	activation := certs.Activation{PreviousBundle: cfg.PendingPreviousBundle, HadPointer: cfg.PendingHadPointer}
	old := cfg
	cfg.AuthToken = cfg.PendingAuthToken
	cfg.RenewalToken = cfg.PendingRenewalToken
	cfg.CredentialExpiresUnix = cfg.PendingExpiresUnix
	cfg.AuthGeneration = cfg.PendingAuthGeneration
	cfg.RenewalStatus = "healthy"
	cfg.RenewalAttempts = 0
	cfg.RenewalLastError = ""
	cfg.RenewalLastSuccessUnix = time.Now().Unix()
	cfg.RenewalNextRetryUnix = 0
	clearPendingCredentialFields(&cfg)
	if err := a.saveCredentialConfig(cfg); err != nil {
		_ = a.certStore.Rollback(activation)
		_ = a.saveCredentialConfig(old)
		return err
	}
	return nil
}

func (a *Agent) recoverCredentials(ctx context.Context, force bool) error {
	if err := a.resumePendingCredentialCutover(); err != nil {
		a.recordCredentialFailure("resume_failed", false)
		return err
	}
	cfg := a.credentialSnapshot()
	expires, err := credentials.Expiry(cfg.AuthMode, cfg.AuthToken, a.certStore, cfg.CredentialExpiresUnix)
	if err != nil && !force {
		return err
	}
	if !force && time.Now().Before(expires) {
		return nil
	}
	if cfg.RenewalToken == "" {
		return fmt.Errorf("control credential expired and no renewal credential is available; re-enroll the agent")
	}
	if cfg.RenewalNextRetryUnix > time.Now().Unix() {
		return fmt.Errorf("credential recovery is waiting for retry backoff")
	}
	cfg, csr, err := a.beginCredentialRequest(time.Now())
	if err != nil {
		a.recordCredentialFailure("prepare_failed", true)
		return err
	}
	endpoint := cfg.RenewalEndpoint
	if endpoint == "" {
		endpoint = defaultRenewalEndpoint(cfg.GRPCEndpoint)
	}
	log.Printf("[credentials] Recovering control credential request_id=%s", cfg.PendingRotationID)
	rotation, err := grpcclient.RenewCredentials(ctx, endpoint, cfg.ServerID, cfg.RenewalToken, cfg.PendingRotationID, normalizedAuthMode(cfg.AuthMode), csr)
	if err != nil {
		a.recordCredentialFailure("recovery_failed", false)
		return err
	}
	if err := a.applyCredentialRotation(rotation); err != nil {
		a.recordCredentialFailure("activation_failed", false)
		return err
	}
	return nil
}

func (a *Agent) recordCredentialFailure(code string, clearPending bool) {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	cfg := *a.cfg
	cfg.RenewalStatus = "failed"
	cfg.RenewalLastError = safeCode(code)
	if cfg.RenewalAttempts < 1 {
		cfg.RenewalAttempts = 1
	}
	delay := renewalBackoff(cfg.RenewalAttempts)
	cfg.RenewalNextRetryUnix = time.Now().Add(delay).Unix()
	requestID := cfg.PendingRotationID
	if clearPending {
		clearPendingCredentialFields(&cfg)
	}
	if err := a.saveCredentialConfig(cfg); err != nil {
		log.Printf("[credentials] Could not persist renewal failure state: %v", err)
	}
	if clearPending && requestID != "" {
		_ = a.certStore.DiscardRotation(requestID)
	}
}

func renewalBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 12 {
		attempt = 12
	}
	delay := time.Minute * time.Duration(1<<(attempt-1))
	if delay > 24*time.Hour {
		return 24 * time.Hour
	}
	return delay
}

func clearPendingCredentialFields(cfg *Config) {
	cfg.PendingRotationID = ""
	cfg.PendingAuthToken = ""
	cfg.PendingRenewalToken = ""
	cfg.PendingExpiresUnix = 0
	cfg.PendingAuthGeneration = 0
	cfg.PendingPreviousBundle = ""
	cfg.PendingHadPointer = false
}

func normalizedAuthMode(mode string) string {
	if mode == "token" {
		return "token"
	}
	return "mtls"
}

func safeCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 100 {
			break
		}
	}
	cleaned := b.String()
	switch cleaned {
	case "activation_failed",
		"agent_stage_failed",
		"auth_mode_mismatch",
		"ca_unavailable",
		"expiry_unavailable",
		"internal_error",
		"invalid_auth_mode",
		"invalid_csr",
		"invalid_request",
		"invalid_server",
		"prepare_failed",
		"recovery_failed",
		"resume_failed",
		"rotation_in_progress",
		"send_failed",
		"server_inactive",
		"unknown":
		return cleaned
	default:
		return "unknown"
	}
}

func defaultRenewalEndpoint(grpcEndpoint string) string {
	host := grpcEndpoint
	if parsedHost, _, err := net.SplitHostPort(grpcEndpoint); err == nil {
		host = parsedHost
	} else if parsed, err := url.Parse("//" + grpcEndpoint); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	}
	host = strings.TrimPrefix(host, "grpc.")
	return "https://" + host + "/api/agent/renew"
}
