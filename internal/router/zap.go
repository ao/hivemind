package router

import (
	"context"
	"log/slog"
	"slices"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// newZap returns a zap logger that writes to log, so certmagic's lines go
// through hivemind's handler, at its level and with its redaction. Each line
// carries certmagic's logger name as "logger".
func newZap(log *slog.Logger) *zap.Logger {
	return zap.New(&slogCore{h: log.Handler()})
}

type slogCore struct {
	h     slog.Handler
	attrs []slog.Attr
}

func slogLevel(l zapcore.Level) slog.Level {
	switch {
	case l <= zapcore.DebugLevel:
		return slog.LevelDebug
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	}
	return slog.LevelError
}

func (c *slogCore) Enabled(l zapcore.Level) bool {
	return c.h.Enabled(context.Background(), slogLevel(l))
}

func (c *slogCore) With(fields []zapcore.Field) zapcore.Core {
	return &slogCore{h: c.h, attrs: append(slices.Clip(c.attrs), fieldAttrs(fields)...)}
}

func (c *slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *slogCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	r := slog.NewRecord(e.Time, slogLevel(e.Level), e.Message, 0)
	if e.LoggerName != "" {
		r.AddAttrs(slog.String("logger", e.LoggerName))
	}
	r.AddAttrs(c.attrs...)
	r.AddAttrs(fieldAttrs(fields)...)
	return c.h.Handle(context.Background(), r)
}

func (c *slogCore) Sync() error { return nil }

// fieldAttrs encodes zap fields as slog attributes, sorted by key.
func fieldAttrs(fields []zapcore.Field) []slog.Attr {
	if len(fields) == 0 {
		return nil
	}
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	keys := make([]string, 0, len(enc.Fields))
	for k := range enc.Fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]slog.Attr, 0, len(keys))
	for _, k := range keys {
		out = append(out, slog.Any(k, enc.Fields[k]))
	}
	return out
}
