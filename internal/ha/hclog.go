package ha

import (
	"context"
	"io"
	"log"
	"log/slog"

	"github.com/hashicorp/go-hclog"
)

// hcLogger writes raft's log lines to a slog.Logger, so they go through
// the redacting handler at the configured level, as every other line does.
// Raft's debug and trace lines are slog's debug.
type hcLogger struct {
	l    *slog.Logger
	name string
	args []any
}

var _ hclog.Logger = (*hcLogger)(nil)

func newHCLogger(l *slog.Logger) hclog.Logger { return &hcLogger{l: l} }

func slogLevel(level hclog.Level) slog.Level {
	switch level {
	case hclog.Trace, hclog.Debug:
		return slog.LevelDebug
	case hclog.Warn:
		return slog.LevelWarn
	case hclog.Error:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (h *hcLogger) Log(level hclog.Level, msg string, args ...any) {
	if level == hclog.Off {
		return
	}
	lv := slogLevel(level)
	ctx := context.Background()
	if !h.l.Enabled(ctx, lv) {
		return
	}
	all := make([]any, 0, len(h.args)+len(args)+2)
	if h.name != "" {
		all = append(all, "raft", h.name)
	}
	all = append(all, h.args...)
	all = append(all, args...)
	h.l.Log(ctx, lv, "raft: "+msg, all...)
}

func (h *hcLogger) Trace(msg string, args ...any) { h.Log(hclog.Trace, msg, args...) }
func (h *hcLogger) Debug(msg string, args ...any) { h.Log(hclog.Debug, msg, args...) }
func (h *hcLogger) Info(msg string, args ...any)  { h.Log(hclog.Info, msg, args...) }
func (h *hcLogger) Warn(msg string, args ...any)  { h.Log(hclog.Warn, msg, args...) }
func (h *hcLogger) Error(msg string, args ...any) { h.Log(hclog.Error, msg, args...) }

func (h *hcLogger) IsTrace() bool { return h.l.Enabled(context.Background(), slog.LevelDebug) }
func (h *hcLogger) IsDebug() bool { return h.l.Enabled(context.Background(), slog.LevelDebug) }
func (h *hcLogger) IsInfo() bool  { return h.l.Enabled(context.Background(), slog.LevelInfo) }
func (h *hcLogger) IsWarn() bool  { return h.l.Enabled(context.Background(), slog.LevelWarn) }
func (h *hcLogger) IsError() bool { return h.l.Enabled(context.Background(), slog.LevelError) }

func (h *hcLogger) ImpliedArgs() []any { return h.args }

func (h *hcLogger) With(args ...any) hclog.Logger {
	return &hcLogger{l: h.l, name: h.name, args: append(append([]any{}, h.args...), args...)}
}

func (h *hcLogger) Name() string { return h.name }

func (h *hcLogger) Named(name string) hclog.Logger {
	n := name
	if h.name != "" {
		n = h.name + "." + name
	}
	return &hcLogger{l: h.l, name: n, args: h.args}
}

func (h *hcLogger) ResetNamed(name string) hclog.Logger {
	return &hcLogger{l: h.l, name: name, args: h.args}
}

// SetLevel does nothing: the level is the slog handler's.
func (h *hcLogger) SetLevel(hclog.Level) {}

func (h *hcLogger) GetLevel() hclog.Level {
	switch {
	case h.IsDebug():
		return hclog.Debug
	case h.IsInfo():
		return hclog.Info
	case h.IsWarn():
		return hclog.Warn
	default:
		return hclog.Error
	}
}

func (h *hcLogger) StandardLogger(opts *hclog.StandardLoggerOptions) *log.Logger {
	return log.New(h.StandardWriter(opts), "", 0)
}

func (h *hcLogger) StandardWriter(*hclog.StandardLoggerOptions) io.Writer { return hcWriter{h} }

type hcWriter struct{ h *hcLogger }

func (w hcWriter) Write(p []byte) (int, error) {
	w.h.Info(string(p))
	return len(p), nil
}
