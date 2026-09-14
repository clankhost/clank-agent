package grpcclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/sysinfo"
	"google.golang.org/grpc"
)

// EnrollResponse is an alias for the proto enrollment response.
type EnrollResponse = clankv1.EnrollResponse

// Enroll calls the AgentEnrollmentService.Enroll RPC via direct connection.
// If caFingerprint is provided (format "sha256:<hex>"), the server certificate
// is verified against it to prevent MITM attacks during enrollment.
func Enroll(endpoint, token, caFingerprint string, info *sysinfo.Info) (*EnrollResponse, error) {
	conn, err := DialEnrollment(endpoint, caFingerprint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return callEnrollRPC(conn, token, info)
}

// EnrollTunnel performs enrollment via REST over HTTPS (Cloudflare Tunnel).
// Cloudflare's gRPC proxy drops HTTP/2 trailing headers after DATA frames,
// breaking gRPC unary responses. REST over HTTPS works reliably through CF.
func EnrollTunnel(endpoint, token string, info *sysinfo.Info) (*EnrollResponse, error) {
	// Derive the HTTPS API base URL from the gRPC tunnel domain.
	// grpc.clank.host → clank.host, grpc.dev.clank.host → dev.clank.host
	host := endpoint
	if len(host) > 5 && host[:5] == "grpc." {
		host = host[5:]
	}
	apiURL := fmt.Sprintf("https://%s/api/agent/enroll", host)

	reqBody := restEnrollRequest{
		Token:    token,
		AuthMode: "token",
		SystemInfo: restSystemInfo{
			Hostname:      info.Hostname,
			OS:            info.OS,
			Arch:          info.Arch,
			CPUCores:      info.CPUCores,
			MemoryBytes:   info.MemoryBytes,
			DockerVersion: info.DockerVersion,
			AgentVersion:  info.AgentVersion,
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enrollment request to %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Detail != "" {
			return nil, fmt.Errorf("enrollment failed: %s", errResp.Detail)
		}
		return nil, fmt.Errorf("enrollment failed: HTTP %d", resp.StatusCode)
	}

	var restResp restEnrollResponse
	if err := json.Unmarshal(respBody, &restResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	// Decode base64 cert fields
	clientCert, err := base64.StdEncoding.DecodeString(restResp.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("decoding client_cert: %w", err)
	}
	clientKey, err := base64.StdEncoding.DecodeString(restResp.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("decoding client_key: %w", err)
	}
	caCert, err := base64.StdEncoding.DecodeString(restResp.CACert)
	if err != nil {
		return nil, fmt.Errorf("decoding ca_cert: %w", err)
	}

	return &EnrollResponse{
		ServerId:              restResp.ServerID,
		ClientCert:            clientCert,
		ClientKey:             clientKey,
		CaCert:                caCert,
		GrpcEndpoint:          restResp.GRPCEndpoint,
		AuthToken:             restResp.AuthToken,
		TunnelEndpoint:        restResp.TunnelEndpoint,
		RegistryUrl:           restResp.RegistryURL,
		RegistryUsername:      restResp.RegistryUsername,
		RegistryPassword:      restResp.RegistryPassword,
		RenewalToken:          restResp.RenewalToken,
		CredentialExpiresUnix: restResp.CredentialExpiresUnix,
		AuthGeneration:        restResp.AuthGeneration,
		RenewalEndpoint:       restResp.RenewalEndpoint,
	}, nil
}

// REST request/response types for tunnel enrollment.

type restSystemInfo struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	CPUCores      int64  `json:"cpu_cores"`
	MemoryBytes   int64  `json:"memory_bytes"`
	DockerVersion string `json:"docker_version"`
	AgentVersion  string `json:"agent_version"`
}

type restEnrollRequest struct {
	Token      string         `json:"token"`
	AuthMode   string         `json:"auth_mode"`
	SystemInfo restSystemInfo `json:"system_info"`
}

type restEnrollResponse struct {
	ServerID              string `json:"server_id"`
	ClientCert            string `json:"client_cert"`
	ClientKey             string `json:"client_key"`
	CACert                string `json:"ca_cert"`
	GRPCEndpoint          string `json:"grpc_endpoint"`
	AuthToken             string `json:"auth_token"`
	TunnelEndpoint        string `json:"tunnel_endpoint"`
	RegistryURL           string `json:"registry_url"`
	RegistryUsername      string `json:"registry_username"`
	RegistryPassword      string `json:"registry_password"`
	RenewalToken          string `json:"renewal_token"`
	CredentialExpiresUnix int64  `json:"credential_expires_unix"`
	AuthGeneration        int64  `json:"auth_generation"`
	RenewalEndpoint       string `json:"renewal_endpoint"`
}

type restRenewRequest struct {
	ServerID     string `json:"server_id"`
	RenewalToken string `json:"renewal_token"`
	RequestID    string `json:"request_id"`
	CSRPem       string `json:"csr_pem"`
	AuthMode     string `json:"auth_mode"`
}

type restRenewResponse struct {
	RequestID             string `json:"request_id"`
	ClientCert            string `json:"client_cert"`
	CACert                string `json:"ca_cert"`
	AuthToken             string `json:"auth_token"`
	RenewalToken          string `json:"renewal_token"`
	CredentialExpiresUnix int64  `json:"credential_expires_unix"`
	AuthGeneration        int64  `json:"auth_generation"`
}

// RenewCredentials recovers an agent whose primary mTLS/JWT credential can no
// longer open the authenticated stream. The renewal token is sent only over
// HTTPS to the narrowly scoped renewal endpoint.
func RenewCredentials(
	ctx context.Context,
	endpoint, serverID, renewalToken, requestID, authMode string,
	csrPEM []byte,
) (*clankv1.CredentialRotation, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("credential renewal endpoint is not configured")
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" || parsedEndpoint.Host == "" {
		return nil, fmt.Errorf("credential renewal endpoint must use HTTPS")
	}
	body, err := json.Marshal(restRenewRequest{
		ServerID: serverID, RenewalToken: renewalToken, RequestID: requestID,
		CSRPem: base64.StdEncoding.EncodeToString(csrPEM), AuthMode: authMode,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling renewal request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating renewal request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("credential renewal request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading credential renewal response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errorResponse struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(responseBody, &errorResponse) == nil && errorResponse.Detail != "" {
			return nil, fmt.Errorf("credential renewal rejected: %s", errorResponse.Detail)
		}
		return nil, fmt.Errorf("credential renewal rejected: HTTP %d", resp.StatusCode)
	}
	var decoded restRenewResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return nil, fmt.Errorf("parsing credential renewal response: %w", err)
	}
	certPEM, err := base64.StdEncoding.DecodeString(decoded.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("decoding renewed client certificate: %w", err)
	}
	caPEM, err := base64.StdEncoding.DecodeString(decoded.CACert)
	if err != nil {
		return nil, fmt.Errorf("decoding renewed CA certificate: %w", err)
	}
	return &clankv1.CredentialRotation{
		RequestId: decoded.RequestID, ClientCert: certPEM, CaCert: caPEM,
		AuthToken: decoded.AuthToken, RenewalToken: decoded.RenewalToken,
		ExpiresUnix: decoded.CredentialExpiresUnix, AuthGeneration: decoded.AuthGeneration,
	}, nil
}

func callEnrollRPC(conn *grpc.ClientConn, token string, info *sysinfo.Info) (*EnrollResponse, error) {
	client := clankv1.NewAgentEnrollmentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Enroll(ctx, &clankv1.EnrollRequest{
		Token: token,
		SystemInfo: &clankv1.SystemInfo{
			Hostname:      info.Hostname,
			Os:            info.OS,
			Arch:          info.Arch,
			CpuCores:      info.CPUCores,
			MemoryBytes:   info.MemoryBytes,
			DockerVersion: info.DockerVersion,
			AgentVersion:  info.AgentVersion,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("enroll RPC: %w", err)
	}

	return resp, nil
}
