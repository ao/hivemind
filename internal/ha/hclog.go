package ha

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
)

// hcLogger writes raft's log lines to a slog.Logger, so they go through
// the redacting handler at the configured level, as every other line does.
// Raft's debug and trace lines are slog's debug. Its lines for a failed RPC
// to another server, and some others that come over and over, are limited
// (peerlog.go).
type hcLogger struct {
	l     *slog.Logger
	name  string
	args  []any
	fails *peerFailures
}

var _ hclog.Logger = (*hcLogger)(nil)

func newHCLogger(l *slog.Logger) *hcLogger { return &hcLogger{l: l, fails: newPeerFailures(l)} }

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
	if lv >= slog.LevelWarn && behindTheLog(msg, args) {
		lv = slog.LevelInfo
	}
	var more []any
	if lv >= slog.LevelWarn {
		var ok bool
		if lv, ok, more = h.fails.limit(lv, msg, args); !ok {
			return
		}
	}
	ctx := context.Background()
	if !h.l.Enabled(ctx, lv) {
		return
	}
	all := make([]any, 0, len(h.args)+len(args)+len(more)+2)
	if h.name != "" {
		all = append(all, "raft", h.name)
	}
	all = append(all, formatArgs(h.args)...)
	all = append(all, formatArgs(args)...)
	all = append(all, more...)
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
	return &hcLogger{l: h.l, name: h.name, args: append(append([]any{}, h.args...), args...), fails: h.fails}
}

func (h *hcLogger) Name() string { return h.name }

func (h *hcLogger) Named(name string) hclog.Logger {
	n := name
	if h.name != "" {
		n = h.name + "." + name
	}
	return &hcLogger{l: h.l, name: n, args: h.args, fails: h.fails}
}

func (h *hcLogger) ResetNamed(name string) hclog.Logger {
	return &hcLogger{l: h.l, name: name, args: h.args, fails: h.fails}
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

// formatArgs gives each hclog.Fmt value its formatted string: slog would
// print the format and its arguments as they are, as in servers="[%+v
// [...]]" (the fifth acceptance run's seventh finding). The rest are
// left as they are. args itself is not changed.
func formatArgs(args []any) []any {
	var out []any
	for i, a := range args {
		f, ok := a.(hclog.Format)
		if !ok {
			continue
		}
		if out == nil {
			out = append([]any(nil), args...)
		}
		out[i] = formatOf(f)
	}
	if out == nil {
		return args
	}
	return out
}

func formatOf(f hclog.Format) string {
	if len(f) == 0 {
		return ""
	}
	s, ok := f[0].(string)
	if !ok {
		return fmt.Sprint([]any(f)...)
	}
	return fmt.Sprintf(s, f[1:]...)
}

// behindTheLog reports a line of raft's for a server that lacks an entry
// another has: a leader's "failed to get log" with "log not found", after
// which it sends the follower a snapshot, as at every server join once the
// log has been compacted, and a follower's "failed to get previous log",
// after which the leader sends earlier entries. Each is info, not an
// error or a warning.
func behindTheLog(msg string, args []any) bool {
	if msg != "failed to get log" && msg != "failed to get previous log" {
		return false
	}
	for i := 0; i+1 < len(args); i += 2 {
		if k, _ := args[i].(string); k == "error" {
			err, _ := args[i+1].(error)
			return errors.Is(err, raft.ErrLogNotFound) || (err != nil && err.Error() == raft.ErrLogNotFound.Error())
		}
	}
	return false
}
