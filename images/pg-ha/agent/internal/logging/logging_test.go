package logging

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"
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
