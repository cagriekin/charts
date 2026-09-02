package dcs

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
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

// A CA file that exists but holds no PEM certificate must be an error, not an empty
// pool: an empty RootCAs silently fails every handshake later, at dial time, in a retry
// loop -- which is the leaderless-behind-a-green-healthz shape the session-failure log
// line exists for. Catch it at boot instead.
func TestEtcdTLSConfigRejectsACAWithNoCertificates(t *testing.T) {
	dir := t.TempDir()
	cert, key, ca := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")
	certPEM, keyPEM := selfSignedPair(t)
	for path, body := range map[string][]byte{cert: certPEM, key: keyPEM, ca: []byte("not a certificate\n")} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := EtcdConfig{CertFile: cert, KeyFile: key, CAFile: ca}.tlsConfig()
	if err == nil {
		t.Fatal("a CA file with no parseable certificate must not produce an empty trust pool")
	}
	if !strings.Contains(err.Error(), "no certificates parsed") {
		t.Errorf("error should say what was wrong with the CA: %v", err)
	}
}

// A valid triplet produces a config pinned to TLS 1.2 or better and carrying both
// halves: the client certificate (etcd authenticates the agent by its CN under
// --client-cert-auth) and the CA pool.
func TestEtcdTLSConfigBuildsAMutualTLSConfig(t *testing.T) {
	dir := t.TempDir()
	cert, key, ca := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")
	certPEM, keyPEM := selfSignedPair(t)
	for path, body := range map[string][]byte{cert: certPEM, key: keyPEM, ca: certPEM} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tc, err := EtcdConfig{CertFile: cert, KeyFile: key, CAFile: ca}.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Certificates) != 1 {
		t.Errorf("no client certificate: etcd's --client-cert-auth would reject the agent")
	}
	if tc.RootCAs == nil {
		t.Error("no CA pool: the agent would trust the platform roots for an internal etcd")
	}
	if tc.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 or better", tc.MinVersion)
	}
}

// #298 review: the revoke rule has two opposite failure modes and this branch shipped each of
// them once. Revoking after a FAILED fence demote hands a peer immediate permission to promote
// beside a postmaster that may still be read-write. Withholding the revoke from a session that
// never WON keeps a queued candidate key alive, which protects nothing and -- because etcd orders
// candidates by create revision -- blocks every peer behind it until the TTL expires. Pure
// function, so the rule is pinned without a live etcd.
func TestShouldRevokeElectionKey(t *testing.T) {
	safe := func() bool { return true }
	unsafe := func() bool { return false }
	for _, tc := range []struct {
		name string
		held bool
		fn   func() bool
		want bool
	}{
		{"held the lock, fence completed -> hand off now", true, safe, true},
		{"held the lock, fence did NOT complete -> hold to TTL", true, unsafe, false},
		// The regression: a follower that never won has no lock to hold. servingRW stays
		// latched by design on a node whose primary wedged, so this is the state an agent roll
		// or a liveness SIGKILL lands in -- and holding there prolongs the leaderlessness the
		// veto exists to shield against.
		{"never won, fence did NOT complete -> still revoke", false, unsafe, true},
		{"never won, fence completed -> revoke", false, safe, true},
		// A caller that does not fence (every backend consumer other than the agent, and the
		// tests) gets the pre-SafeToRelease behaviour.
		{"no callback, held the lock -> revoke", true, nil, true},
		{"no callback, never won -> revoke", false, nil, true},
	} {
		if got := shouldRevoke(tc.held, tc.fn); got != tc.want {
			t.Errorf("%s: shouldRevoke(held=%v) = %v, want %v", tc.name, tc.held, got, tc.want)
		}
	}
}

// A fresh EtcdDCS holds no leadership and names no leader. Leader() in particular must
// return "" rather than panic on the un-stored atomic.Value -- observe() publishes into
// it asynchronously, so the accessor is read before the first write on every boot.
func TestEtcdDCSAccessorsBeforeAnyElection(t *testing.T) {
	e := &EtcdDCS{}
	if e.IsLeader() {
		t.Error("a DCS that has never campaigned must not report leadership")
	}
	if got := e.Leader(); got != "" {
		t.Errorf("Leader() = %q before any observation, want empty", got)
	}
}

// RBACBootstrap validates its inputs before dialling: both are rendered from chart
// values, and a missing one is a configuration error the Job must report by name
// rather than as an etcd connection timeout.
func TestRBACBootstrapValidatesItsInputs(t *testing.T) {
	ctx := context.Background()
	if err := RBACBootstrap(ctx, nil, "", "", "", "root", "", nil); err == nil ||
		!strings.Contains(err.Error(), "ETCD_ENDPOINTS") {
		t.Errorf("no endpoints should name ETCD_ENDPOINTS, got %v", err)
	}
	if err := RBACBootstrap(ctx, []string{"https://e:2379"}, "", "", "", "", "", nil); err == nil ||
		!strings.Contains(err.Error(), "ETCD_RBAC_ROOT_CN") {
		t.Errorf("no root CN should name ETCD_RBAC_ROOT_CN, got %v", err)
	}
	// A half-set TLS triplet is refused here too, before any dial.
	if err := RBACBootstrap(ctx, []string{"https://e:2379"}, "/c", "", "", "root", "", nil); err == nil ||
		!strings.Contains(err.Error(), "together") {
		t.Errorf("a partial TLS triplet should be refused all-or-none, got %v", err)
	}
}

// selfSignedPair returns a throwaway cert/key PEM pair, so the TLS tests exercise the
// real x509 loader rather than a stub.
func selfSignedPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pg-0"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}
