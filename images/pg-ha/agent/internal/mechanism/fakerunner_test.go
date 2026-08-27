package mechanism

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The fake Runner shared by every mechanism test. It lived in repmgr_test.go until #294
// deleted that file; it was never repmgr-specific -- the native tests use it too.

type recordedCall struct {
	name string
	env  []string
	args []string
}

type fakeRunner struct {
	calls   []recordedCall
	failOn  string // if a call's args contain this substring, it errors
	failOut string // combined output returned alongside the error on a failing call
}

func (f *fakeRunner) Run(_ context.Context, env []string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, recordedCall{name: name, env: env, args: args})
	if f.failOn != "" && strings.Contains(strings.Join(args, " "), f.failOn) {
		out := "simulated failure"
		if f.failOut != "" {
			out = f.failOut
		}
		return out, errors.New("exit status 23")
	}
	// A real pg_basebackup creates -D's target directory and copies the whole source
	// PGDATA tree into it (including postgresql.conf); native.Clone's caller (e.g.
	// ReclonePreserving, which renames the old PGDATA aside first) and native.Clone's own
	// trailing Follow call (which reads postgresql.conf) both rely on that, so a
	// successful fake call must mirror it.
	if strings.HasSuffix(name, "pg_basebackup") {
		for i, a := range args {
			if a == "-D" && i+1 < len(args) {
				dir := args[i+1]
				_ = os.MkdirAll(dir, 0o700)
				confPath := filepath.Join(dir, "postgresql.conf")
				if _, err := os.Stat(confPath); os.IsNotExist(err) {
					_ = os.WriteFile(confPath, []byte("# initial\n"), 0o600)
				}
			}
		}
	}
	return "ok", nil
}

func (f *fakeRunner) lastArgs() string {
	if len(f.calls) == 0 {
		return ""
	}
	return strings.Join(f.calls[len(f.calls)-1].args, " ")
}
