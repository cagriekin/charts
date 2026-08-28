package pg

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// scramIterations is PostgreSQL's own default for password_encryption = 'scram-sha-256'
// (the scram_iterations GUC, default 4096). Matched rather than raised so a verifier this
// code mints is indistinguishable from one the server would have produced itself: a higher
// count would still be accepted, but it would make agent-rehashed users measurably slower
// to authenticate than server-hashed ones for no stated reason.
const scramIterations = 4096

// scramSaltLen is 16 bytes, as PostgreSQL's own scram_build_secret uses.
const scramSaltLen = 16

// ScramVerifier builds the RFC 5802 / RFC 7677 SCRAM-SHA-256 secret PostgreSQL stores in
// pg_authid.rolpassword, in the wire format `ALTER USER ... PASSWORD` accepts verbatim:
//
//	SCRAM-SHA-256$<iterations>:<base64 salt>$<base64 StoredKey>:<base64 ServerKey>
//
// The point is that the PLAINTEXT never has to reach the server (#298 review). The rehash
// path previously sent the password as a SQL literal, and while it kept it off argv and out
// of the process table, a top-level `SET` is still logged verbatim under
// log_statement = 'all' or log_min_duration_statement = 0 -- so a cluster with statement
// logging on wrote the superuser and replication passwords into the server log in cleartext,
// where they then reached whatever ships those logs. Hashing here means the strongest thing
// the log can ever contain is the verifier the server was going to store anyway.
//
// A verifier is not a nothing -- it permits an offline dictionary attack and impersonation of
// the server to a client -- so the session that sends it still suppresses statement logging.
// Two independent mitigations, because the first one is a GUC an operator can undo.
func ScramVerifier(password string) (string, error) {
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("scram: read salt: %w", err)
	}
	return scramVerifierWithSalt(password, salt)
}

// scramVerifierWithSalt is the deterministic core, split out so the RFC 7677 known-answer
// vector can be asserted. Hand-rolling SCRAM is only defensible if it is checked against the
// spec's own numbers rather than against itself.
func scramVerifierWithSalt(password string, salt []byte) (string, error) {
	// SaltedPassword := Hi(Normalize(password), salt, i). Normalize is SASLprep (RFC 4013),
	// which this package does NOT implement -- so it refuses to mint a verifier for any
	// password SASLprep could change (ErrNeedsSASLprep, see needsSASLprep).
	//
	// Skipping it silently was a lockout, not a cosmetic gap (#298 review). The server's own
	// scram_build_secret SASLpreps before hashing and libpq SASLpreps before computing
	// ClientProof, so for a password carrying any non-ASCII byte -- NFKC-normalisable
	// characters, a non-ASCII space, a soft hyphen -- the verifier written here is one the
	// user's own password can never match. And it is not recoverable by retrying: rehashSQL
	// is gated on `rolpassword LIKE 'md5%'`, which stops matching the moment the bad verifier
	// lands, so the superuser and the replication user are locked out for good and streaming
	// replication stops cluster-wide. Refusing instead leaves the md5 hash in place, which the
	// agent's own md5-above-scram pg_hba line still authenticates.
	if needsSASLprep(password) {
		return "", ErrNeedsSASLprep
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, scramIterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("scram: pbkdf2: %w", err)
	}
	clientKey := scramHMAC(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := scramHMAC(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		scramIterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

// ErrNeedsSASLprep is returned when a password cannot be hashed here because SASLprep
// might change it and this package does not implement SASLprep. The caller skips the
// re-hash for that user rather than storing a verifier nobody can authenticate against.
var ErrNeedsSASLprep = errors.New("scram: password needs SASLprep normalisation, which this agent does not implement")

// needsSASLprep reports whether SASLprep could change password.
//
// A password made only of printable ASCII (0x21-0x7E plus space) is a SASLprep fixed
// point: NFKC is the identity on it, none of its characters are in the mapped-to-space
// or mapped-to-nothing tables, and none are prohibited. Anything else -- a non-ASCII byte,
// or an ASCII control character (which PostgreSQL's pg_saslprep rejects, then falls back to
// the raw bytes for, a fallback this code would have to mirror exactly) -- is treated as
// "cannot prove it is a fixed point" and refused.
func needsSASLprep(password string) bool {
	for i := 0; i < len(password); i++ {
		if c := password[i]; c < 0x20 || c > 0x7e {
			return true
		}
	}
	return false
}

func scramHMAC(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
