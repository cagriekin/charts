package pg

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The RFC 7677 (SCRAM-SHA-256) example: password "pencil", salt W22ZaJ0SNY7soEsUEjb6gQ==,
// 4096 iterations. Asserted against the spec's own StoredKey/ServerKey rather than against
// a value this code produced, which is the only way a hand-rolled verifier is trustworthy --
// a self-consistent but wrong implementation writes a secret nobody can authenticate against,
// and the symptom would be "the password stopped working after upgrade", not a test failure.
func TestScramVerifierMatchesRFC7677(t *testing.T) {
	salt, err := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	if err != nil {
		t.Fatal(err)
	}
	got, err := scramVerifierWithSalt("pencil", salt)
	if err != nil {
		t.Fatalf("scramVerifierWithSalt: %v", err)
	}
	want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$" +
		"WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
	if got != want {
		t.Errorf("verifier mismatch\n got: %s\nwant: %s", got, want)
	}
}

// The generated form must carry a fresh random salt and the shape PostgreSQL parses.
func TestScramVerifierShapeAndFreshSalt(t *testing.T) {
	a, err := ScramVerifier("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ScramVerifier("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two verifiers for the same password must differ: the salt has to be per-call random")
	}
	for _, v := range []string{a, b} {
		if !strings.HasPrefix(v, "SCRAM-SHA-256$4096:") {
			t.Errorf("unexpected prefix: %s", v)
		}
		// SCRAM-SHA-256$<iter>:<salt>$<stored>:<server> -- two $-separated parts, the second
		// carrying two colon-separated keys.
		parts := strings.Split(v, "$")
		if len(parts) != 3 || len(strings.Split(parts[2], ":")) != 2 {
			t.Errorf("malformed verifier PostgreSQL would reject: %s", v)
		}
		if strings.Contains(v, "hunter2") {
			t.Errorf("the plaintext must never appear in the verifier: %s", v)
		}
	}
}
