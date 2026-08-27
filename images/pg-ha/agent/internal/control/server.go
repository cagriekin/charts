package control

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Request/body/concurrency bounds. The control server shares a process with PID 1
// and the supervised postmaster, so an unbounded client must not be able to pile up
// goroutines or buffers here.
const (
	maxBodyBytes     = 64 << 10
	maxConcurrent    = 16
	readHeaderTO     = 5 * time.Second
	readTO           = 15 * time.Second
	writeTO          = 90 * time.Second
	idleTO           = 120 * time.Second
	maxHeaderBytes   = 64 << 10
	defaultRequestTO = 60 * time.Second
	defaultIntentTO  = 30 * time.Second
	shutdownGrace    = 5 * time.Second
)

// Options configures the control server. Cluster/Node are required; Backups may be
// nil (pgbackrest disabled for this release).
type Options struct {
	Addr     string
	CertFile string
	KeyFile  string
	CAFile   string
	// AllowedCNs, when non-empty, restricts every control route to these client CNs.
	// Empty means any certificate the CA signed, which is already a closed set.
	AllowedCNs []string
	// RestoreAllowedCNs is the separate authz verb for restore. An empty list denies
	// restore to everyone, which is the intended posture when nobody was named.
	RestoreAllowedCNs []string
	// ClusterName is the StatefulSet name; the value a destructive request must echo
	// back in `confirm`.
	ClusterName string
	PodName     string
	Namespace   string

	// RestoreTargetPod is the pod owning the volume the rendered restore Job writes
	// into (<cluster>-<pgbackrest.restore.podOrdinal>), RestorePodOrdinal that
	// ordinal, and RestoreJobName the Job the agent creates. All three come from the
	// chart, so the API confirms them rather than choosing them.
	RestoreTargetPod  string
	RestorePodOrdinal int
	RestoreJobName    string

	Log     *slog.Logger
	Cluster Cluster
	Node    Node
	Backups Backups
	Metrics Metrics

	// RequestTimeout bounds a whole request; IntentTimeout bounds how long a
	// node-local intent may wait for the reconcile loop before the caller gets a 504.
	RequestTimeout time.Duration
	IntentTimeout  time.Duration
}

// Server is the mTLS control API.
type Server struct {
	o    Options
	tls  *tls.Config
	sem  chan struct{}
	http *http.Server

	// cached/cachedStamp memoise the per-handshake reload of the TLS material.
	tlsMu       sync.Mutex
	cached      *tls.Config
	cachedStamp string
}

// New validates the options, loads the TLS material, and builds the server. Every
// failure here is a boot failure by design: a control API that cannot verify clients
// must not start at all, and silently degrading to a weaker mode is exactly the
// outcome the mTLS requirement exists to prevent.
func New(o Options) (*Server, error) {
	if o.Cluster == nil || o.Node == nil {
		return nil, errors.New("control: Cluster and Node are required")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	if o.Metrics == nil {
		o.Metrics = Nop{}
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = defaultRequestTO
	}
	if o.IntentTimeout <= 0 {
		o.IntentTimeout = defaultIntentTO
	}
	// The intent deadline is derived from the request deadline, so if they were equal
	// (which they are on a cloud preset, where IntentTimeout scales to 4x a 15s
	// reconcile interval) the outer one could fire first and return a generic request
	// timeout instead of the specific "restart did not complete, check GET /v1/status".
	// Keep the request budget strictly wider.
	if slack := o.IntentTimeout + 15*time.Second; o.RequestTimeout < slack {
		o.RequestTimeout = slack
	}
	if _, _, err := net.SplitHostPort(o.Addr); err != nil {
		return nil, fmt.Errorf("control: addr %q is not host:port: %w", o.Addr, err)
	}
	s := &Server{o: o, sem: make(chan struct{}, maxConcurrent)}
	base, err := s.loadTLS()
	if err != nil {
		return nil, err
	}
	s.tls = base.Clone()
	// Re-read the material per handshake (mtime-cached, see loadTLS) so a rotated
	// Secret takes effect without a pod restart. Without this, a cert-manager renewal
	// would silently stop working at expiry -- the agent loads certificates at boot and
	// nothing else would notice until an operator needed the API mid-incident.
	s.tls.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return s.currentTLS(), nil
	}
	return s, nil
}

// loadTLS reads the server keypair and the client CA into a fresh config.
func (s *Server) loadTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(s.o.CertFile, s.o.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("control: load server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(s.o.CAFile)
	if err != nil {
		return nil, fmt.Errorf("control: read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		// A readable CA file with no PEM in it is the trap worth catching at boot: client
		// verification would trust nothing and every call would fail at handshake time
		// with no explanation.
		return nil, fmt.Errorf("control: %s contains no PEM certificates", s.o.CAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		// Every route, including the GETs, requires a verified client certificate.
		// There is no anonymous read tier: cluster topology and lease state are not
		// public, and a second auth mode would be a second thing to get wrong.
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13,
	}, nil
}

// currentTLS returns the config to use for a handshake, reloading from disk when any
// of the three files has changed.
//
// A failed reload keeps serving the last good config and logs: a half-written Secret
// update must not take the control API down, which would be a worse outcome than
// briefly using the previous (still valid) material.
func (s *Server) currentTLS() *tls.Config {
	s.tlsMu.Lock()
	defer s.tlsMu.Unlock()
	stamp := s.tlsStamp()
	if s.cached != nil && stamp == s.cachedStamp {
		return s.cached
	}
	cfg, err := s.loadTLS()
	if err != nil {
		if s.cached != nil {
			s.o.Log.Error("control: reloading TLS material failed; continuing with the previously loaded certificates", "err", err)
			return s.cached
		}
		s.o.Log.Error("control: TLS material is unusable", "err", err)
		return s.tls
	}
	cfg.GetConfigForClient = s.tls.GetConfigForClient
	if s.cached != nil {
		s.o.Log.Info("control: reloaded rotated TLS material")
	}
	s.cached, s.cachedStamp = cfg, stamp
	return cfg
}

// tlsStamp is a cheap change detector over the three files (size + modification
// time). Kubernetes replaces a projected Secret's files atomically via a symlink
// swap, so an mtime change is a reliable signal and stat is cheap enough per
// handshake on a control API that sees human-scale traffic.
func (s *Server) tlsStamp() string {
	var sb strings.Builder
	for _, f := range []string{s.o.CertFile, s.o.KeyFile, s.o.CAFile} {
		fi, err := os.Stat(f)
		if err != nil {
			sb.WriteString(f + "=?;")
			continue
		}
		fmt.Fprintf(&sb, "%s=%d@%d;", f, fi.Size(), fi.ModTime().UnixNano())
	}
	return sb.String()
}

// Serve runs the server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	s.http = &http.Server{
		Addr:              s.o.Addr,
		Handler:           s.Handler(),
		TLSConfig:         s.tls,
		ReadHeaderTimeout: readHeaderTO,
		ReadTimeout:       readTO,
		// Derived from the request budget, not a constant: the write deadline is armed when
		// the request header is read, so a fixed 90s would cut off the response to a long
		// intent on a release with a wide reconcile interval (IntentTimeout scales with it),
		// leaving the client with a dropped connection instead of the specific 504.
		WriteTimeout:   s.writeTimeout(),
		IdleTimeout:    idleTO,
		MaxHeaderBytes: maxHeaderBytes,
		// Route net/http's own errors -- above all TLS handshake failures, which are
		// exactly the security signal an operator wants -- into the structured logger
		// instead of the process-wide standard logger.
		ErrorLog: slog.NewLogLogger(s.o.Log.Handler(), slog.LevelWarn),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = s.http.Shutdown(sctx)
	}()
	s.o.Log.Info("control API listening", "addr", s.o.Addr, "mtls", true,
		"allowedCNs", len(s.o.AllowedCNs), "restore", s.o.Backups != nil && s.o.Backups.RestoreEnabled())
	// Certificates come from TLSConfig, so the file arguments are empty.
	if err := s.http.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// writeTimeout is the response-write budget: always wider than the whole request budget,
// with writeTO as a floor for short requests.
func (s *Server) writeTimeout() time.Duration {
	if d := s.o.RequestTimeout + 30*time.Second; d > writeTO {
		return d
	}
	return writeTO
}

// Handler is the full middleware chain plus routes.
func (s *Server) Handler() http.Handler {
	return s.recoverMW(s.limitMW(s.authnMW(s.routes())))
}

type ctxKey int

const identityKey ctxKey = iota

func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

func identityFrom(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey).(Identity)
	return id
}

// recoverMW keeps a handler panic from killing PID 1 -- and with it the postmaster
// this process supervises. The stack goes to the log, never to the client.
func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.o.Log.Error("control handler panic",
					"path", r.URL.Path, "method", r.Method, "panic", fmt.Sprint(v),
					"stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "internal error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// limitMW bounds body size, in-flight requests and request duration.
func (s *Server) limitMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			writeErr(w, http.StatusServiceUnavailable, "too many concurrent control requests", "retry shortly")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		ctx, cancel := context.WithTimeout(r.Context(), s.o.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authnMW turns the verified TLS chain into an Identity. TLS itself has already
// rejected an absent or untrusted client certificate (RequireAndVerifyClientCert), so
// reaching here without one means the server was misconfigured -- fail closed.
func (s *Server) authnMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			s.o.Metrics.IncControlRejected()
			s.audit(r, Identity{}, "", "rejected", "no verified client certificate")
			writeErr(w, http.StatusUnauthorized, "client certificate required", "the control API is mTLS-only")
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		sum := sha256.Sum256(leaf.Raw)
		id := Identity{
			CN:          leaf.Subject.CommonName,
			Fingerprint: hex.EncodeToString(sum[:]),
			Serial:      leaf.SerialNumber.String(),
		}
		s.o.Metrics.IncControlRequest()
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// allowed reports whether id may exercise verb, and on refusal WHICH allowlist refused
// it. Naming the list matters: the two compose (the general one is the door to the API,
// the restore one an extra lock on the destructive verb), so a client listed under
// restore but not generally is refused by the general list -- and a bare "not on the
// allowlist" would send the operator to edit the wrong one.
func (s *Server) allowed(id Identity, verb Verb) (bool, string) {
	if len(s.o.AllowedCNs) > 0 && !containsFold(s.o.AllowedCNs, id.CN) {
		return false, "the client certificate CN is not on ha.agent.control.allowedClientCNs, which gates every control route"
	}
	if verb == VerbRestore {
		// No "allow all" reading of an empty restore list: naming nobody denies
		// everybody, so forgetting the list cannot open the most destructive verb.
		if !containsFold(s.o.RestoreAllowedCNs, id.CN) {
			return false, "the client certificate CN is not on ha.agent.control.restore.allowedClientCNs (restore is a separate verb; a client must be on BOTH that list and allowedClientCNs)"
		}
	}
	return true, ""
}

func containsFold(list []string, v string) bool {
	if v == "" {
		return false
	}
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

// guard wraps a route with its authorization verb and, for mutating verbs, the audit
// line. Every mutating call is audited whatever its outcome -- a denied attempt is at
// least as interesting as a successful one.
func (s *Server) guard(verb Verb, h func(http.ResponseWriter, *http.Request) (status int, detail string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := identityFrom(r.Context())
		if ok, why := s.allowed(id, verb); !ok {
			s.o.Metrics.IncControlRejected()
			s.audit(r, id, verb, "denied", why)
			writeErr(w, http.StatusForbidden, "not authorized for "+string(verb), why)
			return
		}
		status, detail := h(w, r)
		if r.Method != http.MethodGet {
			outcome := "ok"
			if status >= 400 {
				outcome = "error"
			}
			s.audit(r, id, verb, outcome, detail)
		}
	}
}

// audit emits one structured line per mutating or rejected call.
//
// It records the client identity, the verb and the outcome -- and deliberately no
// request-body VALUES beyond ones the handler passes in detail explicitly (a pod
// name, a backup set label). Bodies on this API are small and non-secret today, but
// logging them wholesale is how a future field with a credential in it ends up in
// stdout.
func (s *Server) audit(r *http.Request, id Identity, verb Verb, outcome, detail string) {
	s.o.Log.Info("control request",
		"method", r.Method,
		"path", r.URL.Path,
		"verb", string(verb),
		"outcome", outcome,
		"client_cn", id.CN,
		"client_fingerprint", id.Fingerprint,
		"client_serial", id.Serial,
		"detail", detail,
	)
}

// errorBody is the single error shape every route returns.
type errorBody struct {
	Error string `json:"error"`
	Hint  string `json:"hint,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, hint string) {
	writeJSON(w, status, errorBody{Error: msg, Hint: hint})
}

// decodeBody reads a JSON body into v, rejecting unknown fields so a typo in a
// destructive request (`"forced": true`) fails loudly instead of silently defaulting
// the field it was meant to set. An empty body is accepted as an empty object.
//
// Anything after the object is rejected too: `{"force":true} {"force":false}` would
// otherwise decode the first and discard the rest without a word, which is the same class
// of silent misreading DisallowUnknownFields exists to prevent.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON body: unexpected content after the JSON object")
	}
	return nil
}
