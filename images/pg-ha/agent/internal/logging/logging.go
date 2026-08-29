// Package logging gives the agent one line format everywhere:
//
//	2026-08-30T00:46:00Z [INFO] message key=value key2="value with spaces"
//
// slog's stock TextHandler emits `time=... level=INFO msg=...` instead, which is
// noisier to scan in `kubectl logs`. This handler keeps slog's structured
// key/value attributes (err=, holder=, ...) but fronts every line with a UTC
// RFC3339 timestamp and a bracketed level, so the important part reads left to
// right. It implements slog.Handler in full (WithAttrs/WithGroup/Enabled) so it
// also backs the control server's http.Server ErrorLog via slog.NewLogLogger.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// New returns a logger writing the agent's line format to w at Info and above.
func New(w io.Writer) *slog.Logger {
	return slog.New(NewHandler(w, slog.LevelInfo))
}

// NewHandler builds the handler directly, for callers that want a non-default level.
func NewHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return &handler{w: w, level: level, mu: &sync.Mutex{}}
}

type handler struct {
	w      io.Writer
	level  slog.Leveler
	mu     *sync.Mutex // shared across derived handlers so writes to one w never interleave
	attrs  string      // pre-rendered " key=value" attrs from WithAttrs, already group-prefixed
	groups string      // current group path as "a.b." (or "" at the root)
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.UTC().Format(time.RFC3339))
	b.WriteString(" [")
	b.WriteString(levelName(r.Level))
	b.WriteString("] ")
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.groups, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	var b strings.Builder
	for _, a := range attrs {
		appendAttr(&b, h.groups, a)
	}
	nh := *h
	nh.attrs = h.attrs + b.String()
	return &nh
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.groups = h.groups + name + "."
	return &nh
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// appendAttr writes " key=value" (group-prefixed), recursing into groups. Values
// are quoted only when they would otherwise be ambiguous (contain a space or quote,
// or are empty), matching slog's TextHandler so a log parser stays happy.
func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		gattrs := a.Value.Group()
		if len(gattrs) == 0 {
			return
		}
		gp := prefix
		if a.Key != "" {
			gp = prefix + a.Key + "."
		}
		for _, ga := range gattrs {
			appendAttr(b, gp, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(quoteIfNeeded(a.Value.String()))
}

func quoteIfNeeded(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\n\"=") {
		return strconv.Quote(s)
	}
	return s
}
