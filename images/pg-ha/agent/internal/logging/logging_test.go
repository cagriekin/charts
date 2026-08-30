package logging

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

// lineRE is the contract: an RFC3339 UTC timestamp, a bracketed level, then the message.
var lineRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z \[(INFO|WARN|ERROR|DEBUG)\] `)

func TestLineFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf)
	log.Info("hello world", "holder", "pg-0", "err", "boom")
	line := strings.TrimRight(buf.String(), "\n")
	if !lineRE.MatchString(line) {
		t.Fatalf("line does not match the format contract: %q", line)
	}
	if !strings.Contains(line, "[INFO] hello world holder=pg-0 err=boom") {
		t.Errorf("message/attrs not rendered as expected: %q", line)
	}
}

func TestLevels(t *testing.T) {
	for _, c := range []struct {
		log  func(*slog.Logger)
		want string
	}{
		{func(l *slog.Logger) { l.Warn("w") }, "[WARN] w"},
		{func(l *slog.Logger) { l.Error("e") }, "[ERROR] e"},
	} {
		var buf bytes.Buffer
		c.log(New(&buf))
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("want %q in %q", c.want, buf.String())
		}
	}
}

func TestDebugSuppressedByDefault(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Debug("nope")
	if buf.Len() != 0 {
		t.Errorf("Debug should be suppressed at the default Info level, got %q", buf.String())
	}
}

func TestValuesWithSpacesAreQuoted(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Info("m", "reason", "two words", "plain", "one")
	out := buf.String()
	if !strings.Contains(out, `reason="two words"`) {
		t.Errorf("value with a space must be quoted: %q", out)
	}
	if !strings.Contains(out, "plain=one") {
		t.Errorf("plain value must not be quoted: %q", out)
	}
}

func TestWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf).With("node", "pg-1").WithGroup("lease")
	log.Info("held", "term", 3)
	out := buf.String()
	if !strings.Contains(out, "node=pg-1") {
		t.Errorf("WithAttrs attr missing: %q", out)
	}
	if !strings.Contains(out, "lease.term=3") {
		t.Errorf("group prefix missing: %q", out)
	}
}

// The agent's log is what an operator reads during an incident, and several values it
// prints carry embedded structure -- an error's message, a pg_rewind excerpt, a
// conninfo. Anything ambiguous must be quoted or the line cannot be parsed back;
// anything unambiguous must NOT be, or every field becomes noisier to read.
func TestQuotingCoversEveryAmbiguousValue(t *testing.T) {
	for _, c := range []struct{ val, want string }{
		{"plain", "k=plain"},
		{"two words", `k="two words"`},
		// An empty value is quoted, or `k=` followed by the next field is unparseable.
		{"", `k=""`},
		// An `=` inside the value would otherwise split the field in the wrong place.
		{"host=pg-0 port=5432", `k="host=pg-0 port=5432"`},
		{`say "hi"`, `k="say \"hi\""`},
		{"line\nbreak", `k="line\nbreak"`},
		{"tab\there", `k="tab\there"`},
	} {
		var buf bytes.Buffer
		New(&buf).Info("m", "k", c.val)
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("value %q rendered as %q, want it to contain %q", c.val, strings.TrimRight(buf.String(), "\n"), c.want)
		}
	}
}

// Non-string values keep their natural rendering: an error must print its message
// (not its Go type), a duration its unit, a bool its word. `err` is the single most
// read attribute in this log.
func TestNonStringValuesRenderReadably(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Error("failed", "err", errors.New("boom"), "after", 3*time.Second, "fenced", true, "timeline", 7)
	out := strings.TrimRight(buf.String(), "\n")
	for _, want := range []string{"err=boom", "after=3s", "fenced=true", "timeline=7"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

// A group with no attributes contributes nothing -- not a bare "prefix." and not a
// dangling space.
func TestEmptyGroupContributesNothing(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).WithGroup("lease").Info("held")
	out := strings.TrimRight(buf.String(), "\n")
	if !strings.HasSuffix(out, "[INFO] held") {
		t.Errorf("an attribute-less group leaked into the line: %q", out)
	}
}

// Nested groups compose their prefixes, so two fields named the same under different
// groups stay distinguishable.
func TestNestedGroupsComposeTheirPrefixes(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).WithGroup("dcs").WithGroup("etcd").Info("state", "ttl", 15)
	if !strings.Contains(buf.String(), "dcs.etcd.ttl=15") {
		t.Errorf("nested group prefix missing: %q", buf.String())
	}
}

// Every level renders with its own bracketed tag, including the one slog reaches for
// on a custom level -- the format contract is what the log pipeline's parser keys on.
func TestEveryLevelRendersItsTag(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, slog.LevelDebug))
	for _, c := range []struct {
		lvl slog.Level
		tag string
	}{
		{slog.LevelDebug, "[DEBUG]"},
		{slog.LevelInfo, "[INFO]"},
		{slog.LevelWarn, "[WARN]"},
		{slog.LevelError, "[ERROR]"},
	} {
		buf.Reset()
		log.Log(context.Background(), c.lvl, "m")
		if !strings.Contains(buf.String(), c.tag) {
			t.Errorf("level %v rendered as %q, want %s", c.lvl, strings.TrimRight(buf.String(), "\n"), c.tag)
		}
		if !lineRE.MatchString(strings.TrimRight(buf.String(), "\n")) {
			t.Errorf("level %v broke the line format: %q", c.lvl, buf.String())
		}
	}
}
