package control

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- a throwaway PKI ---

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf certificate. Server leaves carry a 127.0.0.1 SAN so the test
// client can verify the listener.
func (ca *testCA) issue(t *testing.T, cn string, server bool) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tlsFixture is a running control server plus the material to talk to it.
type tlsFixture struct {
	ca   *testCA
	addr string
	h    *harness
}

func startTLSServer(t *testing.T, tweak func(*Options, *harness)) *tlsFixture {
	t.Helper()
	ca := newCA(t, "control-ca")
	dir := t.TempDir()
	srvCert, srvKey := ca.issue(t, "pg-0", true)
	certPath := writeFile(t, dir, "tls.crt", srvCert)
	keyPath := writeFile(t, dir, "tls.key", srvKey)
	caPath := writeFile(t, dir, "ca.crt", ca.pem)

	// Reuse the handler-test fakes, but build the server through New() so the real TLS
	// material loading and validation are exercised.
	h := newHarness(t, tweak)
	o := h.srv.o
	o.CertFile, o.KeyFile, o.CAFile = certPath, keyPath, caPath
	o.Addr = "127.0.0.1:0"
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{
		Handler: srv.Handler(), TLSConfig: srv.tls, ReadHeaderTimeout: readHeaderTO,
		// Handshake failures are expected in these tests; keep them out of the output.
		ErrorLog: slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelWarn),
	}
	go func() { _ = hs.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = hs.Close() })
	return &tlsFixture{ca: ca, addr: ln.Addr().String(), h: h}
}

// client builds an HTTPS client. A nil clientCert means "present no certificate".
func (f *tlsFixture) client(t *testing.T, cn string, signer *testCA, maxVersion uint16) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(f.ca.pem)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if maxVersion != 0 {
		cfg.MaxVersion = maxVersion
	}
	if signer != nil {
		certPEM, keyPEM := signer.issue(t, cn, false)
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

func (f *tlsFixture) url(path string) string { return "https://" + f.addr + path }

// --- the tests ---

func TestMTLSAdmitsAValidClient(t *testing.T) {
	f := startTLSServer(t, nil)
	resp, err := f.client(t, "ops-admin", f.ca, 0).Get(f.url("/v1/status"))
	if err != nil {
		t.Fatalf("a client with a CA-signed certificate should be admitted: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	var got StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Node != "pg-0" {
		t.Errorf("node = %q", got.Node)
	}
}

// No client certificate at all: the TLS handshake itself must fail. There is no
// anonymous tier on this listener, not even for reads.
func TestMTLSRejectsClientWithoutCertificate(t *testing.T) {
	f := startTLSServer(t, nil)
	resp, err := f.client(t, "", nil, 0).Get(f.url("/v1/status"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a client with no certificate must be rejected, got %d", resp.StatusCode)
	}
}

// A certificate from any other CA must not be accepted, however well-formed.
func TestMTLSRejectsForeignCA(t *testing.T) {
	f := startTLSServer(t, nil)
	other := newCA(t, "other-ca")
	resp, err := f.client(t, "ops-admin", other, 0).Get(f.url("/v1/status"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a certificate from an untrusted CA must be rejected, got %d", resp.StatusCode)
	}
}

// Plaintext against the control port must never reach a handler. Go's TLS server
// answers such a request with a bare 400 before any routing, which is the behaviour
// asserted here: no route runs, no identity is established, nothing is served.
func TestControlPortRefusesPlaintext(t *testing.T) {
	f := startTLSServer(t, nil)
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + f.addr + "/v1/status")
	if err != nil {
		return // connection refused/reset is an equally acceptable rejection
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plaintext must not be served: status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "HTTP request to an HTTPS server") {
		t.Errorf("expected the pre-routing TLS rejection, got: %s", body)
	}
	// And it must not be one of our JSON responses: no handler was involved.
	if json.Valid(body) {
		t.Errorf("a plaintext request reached a handler: %s", body)
	}
}

// The listener is TLS 1.3 only.
func TestMTLSRequiresTLS13(t *testing.T) {
	f := startTLSServer(t, nil)
	resp, err := f.client(t, "ops-admin", f.ca, tls.VersionTLS12).Get(f.url("/v1/status"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a TLS 1.2-capped client must be rejected, got %d", resp.StatusCode)
	}
}

// End-to-end proof that authorization keys off the certificate's CN, not a header or
// anything else the client controls in-band.
func TestAuthzUsesTheCertificateCN(t *testing.T) {
	f := startTLSServer(t, func(o *Options, _ *harness) {
		o.AllowedCNs = []string{"ops-admin"}
	})
	admitted, err := f.client(t, "ops-admin", f.ca, 0).Get(f.url("/v1/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admitted.Body.Close() }()
	if admitted.StatusCode != 200 {
		t.Errorf("listed CN: status = %d", admitted.StatusCode)
	}
	denied, err := f.client(t, "intruder", f.ca, 0).Get(f.url("/v1/status"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = denied.Body.Close() }()
	if denied.StatusCode != 403 {
		t.Errorf("unlisted CN: status = %d, want 403", denied.StatusCode)
	}
	// A forged header must not grant anything.
	req, _ := http.NewRequest("GET", f.url("/v1/status"), nil)
	req.Header.Set("X-Client-CN", "ops-admin")
	spoofed, err := f.client(t, "intruder", f.ca, 0).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spoofed.Body.Close() }()
	if spoofed.StatusCode != 403 {
		t.Errorf("a header must not override the certificate identity: status = %d", spoofed.StatusCode)
	}
}

// --- construction-time validation ---

func baseOptions(t *testing.T) Options {
	h := newHarness(t, nil)
	o := h.srv.o
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return o
}

func TestNewRequiresUsableTLSMaterial(t *testing.T) {
	ca := newCA(t, "control-ca")
	dir := t.TempDir()
	certPEM, keyPEM := ca.issue(t, "pg-0", true)
	cert := writeFile(t, dir, "tls.crt", certPEM)
	key := writeFile(t, dir, "tls.key", keyPEM)
	caPath := writeFile(t, dir, "ca.crt", ca.pem)
	// A CA file that is readable but carries no PEM certificate is the trap worth
	// catching: client verification would trust nothing and every call would fail at
	// handshake time with no explanation.
	junkCA := writeFile(t, dir, "junk.crt", []byte("not a certificate\n"))

	cases := []struct {
		name              string
		cert, keyF, caF   string
		addr              string
		wantErrorContains string
	}{
		{"missing cert", filepath.Join(dir, "absent.crt"), key, caPath, "127.0.0.1:0", "server certificate"},
		{"missing CA", cert, key, filepath.Join(dir, "absent-ca.crt"), "127.0.0.1:0", "client CA"},
		{"CA without PEM", cert, key, junkCA, "127.0.0.1:0", "no PEM certificates"},
		{"bad addr", cert, key, caPath, "9201", "host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOptions(t)
			o.CertFile, o.KeyFile, o.CAFile, o.Addr = tc.cert, tc.keyF, tc.caF, tc.addr
			_, err := New(o)
			if err == nil {
				t.Fatal("expected a construction error")
			}
			if !strings.Contains(err.Error(), tc.wantErrorContains) {
				t.Errorf("error should mention %q: %v", tc.wantErrorContains, err)
			}
		})
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	o := baseOptions(t)
	o.Cluster = nil
	if _, err := New(o); err == nil {
		t.Error("a server with no Cluster must not be constructible")
	}
}

// The server must never be built with a permissive client-auth mode, whatever else
// changes around it.
func TestServerTLSConfigIsMutualAndModern(t *testing.T) {
	ca := newCA(t, "control-ca")
	dir := t.TempDir()
	certPEM, keyPEM := ca.issue(t, "pg-0", true)
	o := baseOptions(t)
	o.CertFile = writeFile(t, dir, "tls.crt", certPEM)
	o.KeyFile = writeFile(t, dir, "tls.key", keyPEM)
	o.CAFile = writeFile(t, dir, "ca.crt", ca.pem)
	o.Addr = "127.0.0.1:0"
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if s.tls.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", s.tls.ClientAuth)
	}
	if s.tls.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3", s.tls.MinVersion)
	}
	if s.tls.ClientCAs == nil {
		t.Error("ClientCAs must be set or client certificates could not be verified")
	}
}

// A rotated Secret must take effect without a pod restart. Without this, a
// cert-manager renewal would silently stop working at expiry, and the API would be
// unreachable exactly when an operator needed it.
func TestTLSMaterialIsReloadedOnRotation(t *testing.T) {
	// Build the fixture by hand: the CA files must be rewritable in place.
	ca := newCA(t, "control-ca")
	dir := t.TempDir()
	srvCert, srvKey := ca.issue(t, "pg-0", true)
	o := baseOptions(t)
	o.CertFile = writeFile(t, dir, "tls.crt", srvCert)
	o.KeyFile = writeFile(t, dir, "tls.key", srvKey)
	o.CAFile = writeFile(t, dir, "ca.crt", ca.pem)
	o.Addr = "127.0.0.1:0"
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{
		Handler: srv.Handler(), TLSConfig: srv.tls, ReadHeaderTimeout: readHeaderTO,
		ErrorLog: slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelWarn),
	}
	go func() { _ = hs.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = hs.Close() })
	f := &tlsFixture{ca: ca, addr: ln.Addr().String()}

	// A client signed by a SECOND CA is rejected while only the first is trusted.
	rotated := newCA(t, "rotated-ca")
	if resp, cerr := f.client(t, "ops-admin", rotated, 0).Get(f.url("/v1/status")); cerr == nil {
		_ = resp.Body.Close()
		t.Fatal("a client from an untrusted CA must be rejected before rotation")
	}

	// Rotate: the mounted ca.crt now carries both CAs (what a CA roll looks like).
	both := append(append([]byte{}, ca.pem...), rotated.pem...)
	// Ensure the modification time actually differs on filesystems with coarse mtimes.
	if err := os.Chtimes(o.CAFile, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.CAFile, both, 0o600); err != nil {
		t.Fatal(err)
	}

	// The same client is now admitted, with no restart in between.
	resp, err := f.client(t, "ops-admin", rotated, 0).Get(f.url("/v1/status"))
	if err != nil {
		t.Fatalf("a client from the rotated-in CA should be admitted after rotation: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// A broken rotation (a truncated or half-written Secret) must not take the API down:
// the previously loaded material keeps working.
func TestBrokenRotationKeepsServing(t *testing.T) {
	ca := newCA(t, "control-ca")
	dir := t.TempDir()
	srvCert, srvKey := ca.issue(t, "pg-0", true)
	o := baseOptions(t)
	o.CertFile = writeFile(t, dir, "tls.crt", srvCert)
	o.KeyFile = writeFile(t, dir, "tls.key", srvKey)
	o.CAFile = writeFile(t, dir, "ca.crt", ca.pem)
	o.Addr = "127.0.0.1:0"
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	// Prime the cache, then corrupt the CA file.
	_ = srv.currentTLS()
	if err := os.WriteFile(o.CAFile, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := srv.currentTLS()
	if cfg == nil || cfg.ClientCAs == nil {
		t.Fatal("a failed reload must fall back to the last good config, not to nil")
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("the fallback must not weaken client authentication")
	}
}

// The intent deadline hangs off the request deadline, so the request budget must stay
// strictly wider -- otherwise a slow restart returns a generic request timeout instead of
// the specific "check GET /v1/status" answer.
func TestRequestTimeoutStaysWiderThanIntentTimeout(t *testing.T) {
	ca := newCA(t, "control-ca")
	dir := t.TempDir()
	certPEM, keyPEM := ca.issue(t, "pg-0", true)
	mk := func(req, intent time.Duration) *Server {
		o := baseOptions(t)
		o.CertFile = writeFile(t, dir, "tls.crt", certPEM)
		o.KeyFile = writeFile(t, dir, "tls.key", keyPEM)
		o.CAFile = writeFile(t, dir, "ca.crt", ca.pem)
		o.Addr = "127.0.0.1:0"
		o.RequestTimeout, o.IntentTimeout = req, intent
		s, err := New(o)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// Equal budgets (the cloud-preset case: 4 x 15s reconcile interval) must be widened.
	s := mk(60*time.Second, 60*time.Second)
	if s.o.RequestTimeout <= s.o.IntentTimeout {
		t.Errorf("RequestTimeout=%s must exceed IntentTimeout=%s", s.o.RequestTimeout, s.o.IntentTimeout)
	}
	// An already-wide request budget is left alone.
	s2 := mk(300*time.Second, 30*time.Second)
	if s2.o.RequestTimeout != 300*time.Second {
		t.Errorf("RequestTimeout = %s, want the configured 300s", s2.o.RequestTimeout)
	}
	// Defaults are consistent with each other.
	s3 := mk(0, 0)
	if s3.o.RequestTimeout <= s3.o.IntentTimeout {
		t.Errorf("defaults: RequestTimeout=%s IntentTimeout=%s", s3.o.RequestTimeout, s3.o.IntentTimeout)
	}
}

// The response-write deadline is armed when the request header is read, so a fixed value
// would cut off the response to a long intent on a release with a wide reconcile interval
// (IntentTimeout scales with it) -- the client would see a dropped connection instead of
// the specific 504.
func TestWriteTimeoutExceedsTheRequestBudget(t *testing.T) {
	ca := newCA(t, "control-ca")
	dir := t.TempDir()
	certPEM, keyPEM := ca.issue(t, "pg-0", true)
	mk := func(intent time.Duration) *Server {
		o := baseOptions(t)
		o.CertFile = writeFile(t, dir, "tls.crt", certPEM)
		o.KeyFile = writeFile(t, dir, "tls.key", keyPEM)
		o.CAFile = writeFile(t, dir, "ca.crt", ca.pem)
		o.Addr = "127.0.0.1:0"
		o.RequestTimeout, o.IntentTimeout = 0, intent
		s, err := New(o)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// A wide reconcile interval (30s) yields IntentTimeout 120s; the write deadline must
	// outlast the whole request budget, not the old fixed 90s.
	s := mk(120 * time.Second)
	if s.writeTimeout() <= s.o.RequestTimeout {
		t.Errorf("writeTimeout %s must exceed RequestTimeout %s", s.writeTimeout(), s.o.RequestTimeout)
	}
	if s.writeTimeout() <= s.o.IntentTimeout {
		t.Errorf("writeTimeout %s must exceed IntentTimeout %s", s.writeTimeout(), s.o.IntentTimeout)
	}
	// Short requests keep the floor rather than shrinking below it.
	if got := mk(5 * time.Second).writeTimeout(); got < writeTO {
		t.Errorf("writeTimeout = %s, want at least the %s floor", got, writeTO)
	}
}
