package main

import (
	"io"
	"log/slog"
	"time"
)

func newLogger(output io.Writer, level string, debug bool) *slog.Logger {
	minimum := slog.LevelInfo
	if level == "debug" || debug {
		minimum = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: minimum, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Time(slog.TimeKey, a.Value.Time().In(time.UTC))
		}
		return a
	}}))
}
