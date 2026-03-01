package selfupdate

import _ "embed"

// SigningPublicKey is the ECDSA P-256 public key used to verify binary signatures
// during self-update. Embedded at compile time from agent_signing_pubkey.pem.
//
// If empty (e.g., in dev builds without the pubkey file), signature verification
// is skipped with a warning log. This should NEVER happen in production binaries.
//
//go:embed agent_signing_pubkey.pem
var SigningPublicKey []byte
