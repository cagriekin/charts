package dcs

import (
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
