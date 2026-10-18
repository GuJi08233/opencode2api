package telemetry

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"opencode2api/internal/config"
)

func NewStructuredLogger(level *slog.LevelVar, redactor *config.SecretRedactor) *slog.Logger {
	return newStructuredLogger(os.Stdout, level, redactor)
}

func newStructuredLogger(output io.Writer, level *slog.LevelVar, redactor *config.SecretRedactor) *slog.Logger {
	base := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			return sanitizeLogAttr(redactor, attr)
		},
	})
	return slog.New(base)
}

func sanitizeLogAttr(redactor *config.SecretRedactor, attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	lower := strings.ToLower(attr.Key)
	if strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") || strings.Contains(lower, "secret") {
		return slog.String(attr.Key, "***")
	}
	if redactor == nil {
		return attr
	}
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(redactor.String(attr.Value.String()))
	case slog.KindAny:
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(redactor.String(err.Error()))
		}
	}
	return attr
}

func SetLogLevel(level *slog.LevelVar, value string) {
	switch value {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}
}
