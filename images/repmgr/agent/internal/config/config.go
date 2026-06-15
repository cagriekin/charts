// Package config loads and validates the agent's configuration from the
// environment at boot. Per the fail-fast rule, every required variable is
// validated up front and a single error lists everything missing/invalid; the
// agent terminates rather than starting half-configured.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the validated agent configuration.
type Config struct {
	PodName   string // this pod's name — the Lease holder identity
	Namespace string

	LeaseName         string
	LeaseDuration     time.Duration
	RenewDeadline     time.Duration
	RetryPeriod       time.Duration
	ReconcileInterval time.Duration

	HeadlessService string // for building peer FQDNs <pod>.<headless>
	NodeCount       int    // total pods (replicaCount+1) for peer enumeration
	MasterService   string // write Service whose selector the agent patches
	MarkerName      string // durable primary-marker ConfigMap
	PodSelector     string // label selector for the postgresql pods (pg-role labeling)

	RepmgrUser     string
	RepmgrDB       string
	RepmgrPassword string
	PGDATA         string

	DCSBackend       string // "kubernetes" | "etcd"
	SplitBrainAction string // "log" | "fence"
}

type loader struct {
	get     func(string) string
	missing []string
	invalid []string
}

func (l *loader) str(key string) string {
	v := l.get(key)
	if v == "" {
		l.missing = append(l.missing, key)
	}
	return v
}

func (l *loader) dur(key string) time.Duration {
	v := l.get(key)
	if v == "" {
		l.missing = append(l.missing, key)
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.invalid = append(l.invalid, fmt.Sprintf("%s=%q (%v)", key, v, err))
	}
	return d
}

func (l *loader) intv(key string) int {
	v := l.get(key)
	if v == "" {
		l.missing = append(l.missing, key)
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.invalid = append(l.invalid, fmt.Sprintf("%s=%q (%v)", key, v, err))
	}
	return n
}

// Load reads the config using get (os.Getenv in production). It returns an error
// listing every missing/invalid variable so misconfiguration is fixed in one pass.
func Load(get func(string) string) (*Config, error) {
	l := &loader{get: get}
	c := &Config{
		PodName:           l.str("POD_NAME"),
		Namespace:         l.str("NAMESPACE"),
		LeaseName:         l.str("LEASE_NAME"),
		LeaseDuration:     l.dur("LEASE_DURATION"),
		RenewDeadline:     l.dur("RENEW_DEADLINE"),
		RetryPeriod:       l.dur("RETRY_PERIOD"),
		ReconcileInterval: l.dur("RECONCILE_INTERVAL"),
		HeadlessService:   l.str("HEADLESS_SERVICE"),
		NodeCount:         l.intv("REPMGR_NODE_COUNT"),
		MasterService:     l.str("MASTER_SERVICE"),
		MarkerName:        l.str("PRIMARY_MARKER"),
		PodSelector:       l.str("POD_SELECTOR"),
		RepmgrUser:        l.str("REPMGR_USER"),
		RepmgrDB:          l.str("REPMGR_DB"),
		RepmgrPassword:    l.str("REPMGR_PASSWORD"),
		PGDATA:            l.str("PGDATA"),
		DCSBackend:        l.str("DCS_BACKEND"),
		SplitBrainAction:  l.str("SPLIT_BRAIN_ACTION"),
	}

	// Cross-field validation that the lease timings are internally consistent
	// (client-go requires LeaseDuration > RenewDeadline > RetryPeriod).
	if c.LeaseDuration > 0 && c.RenewDeadline > 0 && c.RetryPeriod > 0 {
		if !(c.LeaseDuration > c.RenewDeadline && c.RenewDeadline > c.RetryPeriod) {
			l.invalid = append(l.invalid, fmt.Sprintf("lease timings must satisfy LeaseDuration(%s) > RenewDeadline(%s) > RetryPeriod(%s)",
				c.LeaseDuration, c.RenewDeadline, c.RetryPeriod))
		}
	}
	// Validate enums only when present (an empty value is already a "missing" error).
	if c.DCSBackend != "" && c.DCSBackend != "kubernetes" && c.DCSBackend != "etcd" {
		l.invalid = append(l.invalid, fmt.Sprintf("DCS_BACKEND=%q (want kubernetes|etcd)", c.DCSBackend))
	}
	if c.SplitBrainAction != "" && c.SplitBrainAction != "log" && c.SplitBrainAction != "fence" {
		l.invalid = append(l.invalid, fmt.Sprintf("SPLIT_BRAIN_ACTION=%q (want log|fence)", c.SplitBrainAction))
	}

	if len(l.missing) > 0 || len(l.invalid) > 0 {
		return nil, fmt.Errorf("config error: missing [%s]; invalid [%s]",
			strings.Join(l.missing, ", "), strings.Join(l.invalid, "; "))
	}
	return c, nil
}

// FromEnv loads the config from the process environment.
func FromEnv() (*Config, error) { return Load(os.Getenv) }

// String renders the config with the repmgr password redacted, so logging the
// config (e.g. at startup) never leaks the secret. fmt uses this for %v/%s/%+v.
func (c Config) String() string {
	return fmt.Sprintf("Config{PodName:%s Namespace:%s LeaseName:%s "+
		"LeaseDuration:%s RenewDeadline:%s RetryPeriod:%s ReconcileInterval:%s "+
		"HeadlessService:%s NodeCount:%d MasterService:%s MarkerName:%s PodSelector:%q "+
		"RepmgrUser:%s RepmgrDB:%s RepmgrPassword:*** PGDATA:%s DCSBackend:%s SplitBrainAction:%s}",
		c.PodName, c.Namespace, c.LeaseName,
		c.LeaseDuration, c.RenewDeadline, c.RetryPeriod, c.ReconcileInterval,
		c.HeadlessService, c.NodeCount, c.MasterService, c.MarkerName, c.PodSelector,
		c.RepmgrUser, c.RepmgrDB, c.PGDATA, c.DCSBackend, c.SplitBrainAction)
}
