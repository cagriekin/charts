package childenv

import (
	"slices"
	"strings"
	"testing"
)

func TestFilteredStripsPasswordVars(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"REPMGR_PASSWORD=secret1",
		"POSTGRES_PASSWORD=secret2",
		"SOME_PASSWORD_THING=secret3",
		"PGDATA=/data",
	}
	out := Filtered(base, nil)
	for _, kv := range out {
		if strings.Contains(kv, "PASSWORD") {
			t.Errorf("credential var survived filtering: %q", kv)
		}
	}
	if !slices.Contains(out, "PATH=/usr/bin") || !slices.Contains(out, "PGDATA=/data") {
		t.Errorf("non-credential vars were dropped: %v", out)
	}
}

func TestFilteredKeepsPGPASSWORD(t *testing.T) {
	// PGPASSWORD is the mechanism children authenticate with; it must NOT be stripped
	// from base, and an explicit one in extra must win.
	base := []string{"PGPASSWORD=inherited", "REPMGR_PASSWORD=drop"}
	out := Filtered(base, []string{"PGPASSWORD=explicit"})
	if slices.Contains(out, "REPMGR_PASSWORD=drop") {
		t.Fatal("REPMGR_PASSWORD survived")
	}
	// Both PGPASSWORD entries may be present; the last (extra) wins under exec semantics.
	if out[len(out)-1] != "PGPASSWORD=explicit" {
		t.Errorf("extra PGPASSWORD must be appended last, got %v", out)
	}
}

func TestFilteredAppendsExtra(t *testing.T) {
	out := Filtered([]string{"A=1"}, []string{"B=2", "C=3"})
	if !slices.Equal(out, []string{"A=1", "B=2", "C=3"}) {
		t.Errorf("unexpected order/content: %v", out)
	}
}

func TestFilteredMalformedEntryWithoutEquals(t *testing.T) {
	// A bare token with no '=' should be treated by name and kept unless it carries PASSWORD.
	out := Filtered([]string{"BAREWORD", "STRAYPASSWORD"}, nil)
	if !slices.Contains(out, "BAREWORD") {
		t.Errorf("bare non-credential token dropped: %v", out)
	}
	if slices.Contains(out, "STRAYPASSWORD") {
		t.Errorf("bare credential-named token kept: %v", out)
	}
}
