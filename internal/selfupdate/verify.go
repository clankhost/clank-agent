package selfupdate

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
)

// VerifySignature verifies an ECDSA P-256 signature over a SHA-256 hash.
// The signature is hex-encoded ASN.1 DER format.
// The hash is the hex-encoded SHA-256 of the file being verified.
//
// Returns nil if the signature is valid, or an error describing the failure.
func VerifySignature(fileHashHex, signatureHex string, pubKeyPEM []byte) error {
	if len(pubKeyPEM) == 0 {
		return fmt.Errorf("no signing public key embedded in binary")
	}

	// Parse the PEM-encoded public key
	block, _ := pem.Decode(pubKeyPEM)
	if block == nil {
		return fmt.Errorf("failed to decode PEM public key")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse public key: %w", err)
	}

	ecdsaPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("public key is not ECDSA (got %T)", pub)
	}

	// Decode the file's SHA-256 hash
	fileHash, err := hex.DecodeString(fileHashHex)
	if err != nil {
		return fmt.Errorf("invalid file hash hex: %w", err)
	}

	// Decode the signature
	sig, err := hex.DecodeString(signatureHex)
	if err != nil {
		return fmt.Errorf("invalid signature hex: %w", err)
	}

	// ECDSA verification: the signature was created over the raw SHA-256 hash bytes
	if !ecdsa.VerifyASN1(ecdsaPub, fileHash, sig) {
		return fmt.Errorf("signature verification failed — binary may have been tampered with")
	}

	return nil
}

// HashAndVerify computes the SHA-256 of a file and verifies the ECDSA signature.
func HashAndVerify(filePath, signatureHex string, pubKeyPEM []byte) (string, error) {
	hash, err := fileSHA256(filePath)
	if err != nil {
		return "", fmt.Errorf("computing file hash: %w", err)
	}

	// Recompute as raw bytes for verification
	hashBytes, _ := hex.DecodeString(hash)
	_ = hashBytes // fileSHA256 returns hex, VerifySignature takes hex

	if err := VerifySignature(hash, signatureHex, pubKeyPEM); err != nil {
		return hash, err
	}

	return hash, nil
}

// signingKeyHash returns the SHA-256 fingerprint of the embedded public key
// for logging/debugging purposes.
func signingKeyFingerprint(pubKeyPEM []byte) string {
	h := sha256.Sum256(pubKeyPEM)
	return hex.EncodeToString(h[:8]) // First 8 bytes = 16 hex chars
}
