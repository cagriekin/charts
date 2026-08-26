package dcs

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewEtcdDCSValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  EtcdConfig
		want string // substring of the expected error ("" = no error)
	}{
		{"no endpoints", EtcdConfig{Prefix: "/p", TTLSeconds: 15}, "no endpoints"},
		{"no prefix", EtcdConfig{Endpoints: []string{"http://x:2379"}, TTLSeconds: 15}, "no key prefix"},
		{"bad ttl", EtcdConfig{Endpoints: []string{"http://x:2379"}, Prefix: "/p", TTLSeconds: 0}, "TTLSeconds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewEtcdDCS(c.cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestEtcdTLSConfig(t *testing.T) {
	// None set -> plaintext (nil, no error).
	if tc, err := (EtcdConfig{}).tlsConfig(); err != nil || tc != nil {
		t.Errorf("no TLS files => (nil, nil), got (%v, %v)", tc, err)
	}
	// Partial -> error (all-or-none).
	if _, err := (EtcdConfig{CertFile: "/c"}).tlsConfig(); err == nil || !strings.Contains(err.Error(), "together") {
		t.Errorf("partial TLS must error all-or-none, got %v", err)
	}
	// All set but unreadable -> a load error (not a silent plaintext fallback).
	if _, err := (EtcdConfig{CertFile: "/nope/c", KeyFile: "/nope/k", CAFile: "/nope/ca"}).tlsConfig(); err == nil {
		t.Error("all TLS files set but missing must error, not fall back to plaintext")
	}
}

func TestEtcdRetryPeriodDefault(t *testing.T) {
	e := &EtcdDCS{}
	if e.retryPeriod() != 2*time.Second {
		t.Errorf("zero RetryPeriod must default to 2s, got %s", e.retryPeriod())
	}
	e.cfg.RetryPeriod = 4 * time.Second
	if e.retryPeriod() != 4*time.Second {
		t.Errorf("RetryPeriod = %s, want 4s", e.retryPeriod())
	}
}

// EtcdDCS must satisfy the DCS interface (compile-time check).
var _ DCS = (*EtcdDCS)(nil)

// A session that lapses while Campaign is still blocked must NOT yield leadership.
// etcd's Election.Campaign does not abort on session expiry (it only watches lower
// revisions and the ctx), and it returns nil once those keys are gone -- so an
// unguarded caller declares leadership holding no lease and no key (#326).
func TestCampaignGuardedRejectsWinOnLapsedSession(t *testing.T) {
	sessDone := make(chan struct{})
	release := make(chan struct{})

	// Campaign blocks (as waitDeletes does), then returns nil "you are leader"
	// AFTER the session has already lapsed.
	campaign := func(ctx context.Context) error {
		<-release
		return nil
	}

	got := make(chan bool, 1)
	go func() { got <- campaignGuarded(context.Background(), sessDone, campaign) }()

	close(sessDone) // lease expires while campaigning
	close(release)  // waitDeletes then returns nil

	select {
	case won := <-got:
		if won {
			t.Fatal("declared leadership on a lapsed session: no lease, no key, not leader")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("campaignGuarded did not return")
	}
}

// The session lapsing must unblock a campaign that would otherwise wait forever,
// so the Run loop can start a fresh iteration instead of becoming a zombie
// candidate (blocked in Campaign, key already deleted by etcd) (#326).
func TestCampaignGuardedCancelsCampaignWhenSessionLapses(t *testing.T) {
	sessDone := make(chan struct{})
	campaignCtxDone := make(chan struct{}, 1)

	campaign := func(ctx context.Context) error {
		<-ctx.Done() // must be cancelled by the session lapsing
		campaignCtxDone <- struct{}{}
		return ctx.Err()
	}

	got := make(chan bool, 1)
	go func() { got <- campaignGuarded(context.Background(), sessDone, campaign) }()

	close(sessDone)

	select {
	case <-campaignCtxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session lapse did not cancel the campaign context (zombie candidate)")
	}
	if won := <-got; won {
		t.Fatal("won leadership despite a lapsed session")
	}
}

// The normal path: campaign wins while the session is alive -> leadership granted.
func TestCampaignGuardedGrantsLeadershipOnLiveSession(t *testing.T) {
	sessDone := make(chan struct{}) // never closed: session stays alive
	campaign := func(ctx context.Context) error { return nil }

	if !campaignGuarded(context.Background(), sessDone, campaign) {
		t.Fatal("a campaign won on a live session must grant leadership")
	}
}

// The guard must NOT wait for Campaign to unwind once the session has lapsed.
// etcd's cancel path calls Resign on the CLIENT context -- which has no deadline and
// is only cancelled by Client.Close() -- and clientv3 defaults to WaitForReady(true),
// so that Txn blocks for the entire remainder of an etcd outage. Blocking on it would
// stop runElection returning and keep the Run loop from starting a fresh iteration,
// which is precisely what the guard exists to guarantee (#326).
func TestCampaignGuardedDoesNotWaitForCampaignUnwind(t *testing.T) {
	sessDone := make(chan struct{})
	unwind := make(chan struct{}) // never closed during the assertion: Resign is stuck

	campaign := func(ctx context.Context) error {
		<-unwind
		return ctx.Err()
	}

	got := make(chan bool, 1)
	go func() { got <- campaignGuarded(context.Background(), sessDone, campaign) }()

	close(sessDone) // lease lapses mid-campaign, mid-partition

	select {
	case won := <-got:
		if won {
			t.Fatal("won leadership on a lapsed session")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guard blocked waiting for Campaign to unwind; the Run loop cannot re-contend")
	}
	close(unwind)
}
