package validate

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex returns the hex-encoded sha256 of b. Used by the hash-integrity
// check to recompute a file's hash and assert it matches the manifest.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
