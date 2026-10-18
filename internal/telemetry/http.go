// Package telemetry records request outcomes, metrics, and structured logs.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/protocol"
)

type RequestMeta struct {
	Model          string
	Protocol       protocol.Protocol
	Request        string
	Session        string
	Proxy          string
	Attempts       int
	Legs           []string
	Stop           string
	Tail           string
	Stream         bool
	ServerKeyID    string
	ToolAlias      string
	Usage          protocol.Usage
	UsageReported  bool
	AttemptOutcome string
	Outcome        string
}

type requestMetaKey struct{}

func WithRequestMeta(ctx context.Context, meta *RequestMeta) context.Context {
	return context.WithValue(ctx, requestMetaKey{}, meta)
}

func MetaFromContext(ctx context.Context) *RequestMeta {
	meta, _ := ctx.Value(requestMetaKey{}).(*RequestMeta)
	return meta
}

func MetaFromRequest(r *http.Request) *RequestMeta {
	return MetaFromContext(r.Context())
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func Middleware(monitor *Monitor, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		meta := &RequestMeta{}
		r = r.WithContext(WithRequestMeta(r.Context(), meta))
		writer := &statusWriter{ResponseWriter: w}
		if monitor != nil {
			monitor.active.Add(1)
		}
		defer func() {
			if monitor != nil {
				monitor.active.Add(-1)
			}
			status := writer.status
			if status == 0 {
				status = http.StatusOK
			}
			duration := time.Since(started)
			if monitor != nil {
				monitor.Record(status, meta)
			}
			outcome := meta.Outcome
			if outcome == "" {
				outcome = requestOutcome(status, meta.Attempts > 0)
			}
			if meta.Attempts > 0 {
				logger.Info("request routed", "component", "http", "event", "request_routed", "method", r.Method,
					"path", r.URL.Path, "status", status, "duration_ms", duration.Milliseconds(), "request_id", meta.Request,
					"model", meta.Model, "session", meta.Session, "server_key_id", meta.ServerKeyID,
					"proxy", meta.Proxy, "attempts", meta.Attempts, "legs", strings.Join(meta.Legs, " "), "stream", meta.Stream, "outcome", outcome,
					"stop", meta.Stop, "in", meta.Usage.Input, "out", meta.Usage.Output, "reasoning_tokens", meta.Usage.Reasoning, "cached", meta.Usage.Cached, "tail", meta.Tail)
			} else if logger.Enabled(r.Context(), slog.LevelDebug) {
				logger.Debug("request completed", "component", "http", "event", "request_complete", "method", r.Method,
					"path", r.URL.Path, "status", status, "duration_ms", duration.Milliseconds(), "bytes", writer.bytes,
					"request_id", meta.Request, "model", meta.Model, "outcome", outcome)
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

func requestOutcome(status int, routed bool) string {
	if !routed {
		return "not_routed"
	}
	if status >= 200 && status < 400 {
		return "success"
	}
	if status >= 400 && status < 500 {
		return "client_error"
	}
	if status >= 500 {
		return "server_error"
	}
	return "unknown"
}
