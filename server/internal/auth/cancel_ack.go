package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// cancelAckDomain separates cancel-ack proofs from every other HMAC made with
// the server secret.
const cancelAckDomain = "multica/cancel-ack/v1"

// CancelAckProof is the value the server hands the claiming daemon in the claim
// response and the daemon echoes in its cancel-ack. The runtime owner's PAT/JWT
// authenticates both the daemon and the owner's other clients, so that
// credential cannot show the runner stopped; only the process that received the
// claim response holds this value.
func CancelAckProof(taskID, runtimeID string) string {
	mac := hmac.New(sha256.New, JWTSecret())
	mac.Write([]byte(cancelAckDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(taskID))
	mac.Write([]byte{0})
	mac.Write([]byte(runtimeID))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyCancelAckProof reports whether proof was minted for this task and
// runtime. An empty proof never verifies.
func VerifyCancelAckProof(taskID, runtimeID, proof string) bool {
	if proof == "" {
		return false
	}
	return hmac.Equal([]byte(CancelAckProof(taskID, runtimeID)), []byte(proof))
}
