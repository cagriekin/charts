package main

import (
	"testing"

	"github.com/cagriekin/pg-ha-agent/internal/pg"
	"github.com/cagriekin/pg-ha-agent/internal/reconcile"
)

func TestDesiredRoleLabels(t *testing.T) {
	peers := []reconcile.PeerState{
		{Name: "pg-1", Reachable: true, Role: pg.RoleStandby},  // -> standby (joins read-only Service)
		{Name: "pg-2", Reachable: true, Role: pg.RolePrimary},  // a second primary -> orphan (out of reads)
		{Name: "pg-3", Reachable: false, Role: pg.RoleUnknown}, // unreachable -> omitted (label untouched)
	}
	got := desiredRoleLabels("pg-0", peers)

	want := map[string]string{"pg-0": "primary", "pg-1": "standby", "pg-2": "orphan"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["pg-3"]; ok {
		t.Error("unreachable peer must be omitted so its label is left untouched (#140)")
	}
}

func TestNodeIDAndBaseName(t *testing.T) {
	if got := baseName("my-pg-0"); got != "my-pg" {
		t.Errorf("baseName = %q, want my-pg", got)
	}
	if got := nodeID("my-pg-3"); got != 1003 {
		t.Errorf("nodeID = %d, want 1003", got)
	}
}
