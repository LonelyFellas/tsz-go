package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestContextHandler_StampsRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(&contextHandler{Handler: slog.NewJSONHandler(&buf, nil)})

	ctx := WithRequestID(context.Background(), "req-123")
	logger.InfoContext(ctx, "hello")

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if m["request_id"] != "req-123" {
		t.Errorf("request_id = %v, want req-123", m["request_id"])
	}
}

func TestContextHandler_NoRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(&contextHandler{Handler: slog.NewJSONHandler(&buf, nil)})

	// A bare context carries no ID, so no request_id field should appear.
	logger.InfoContext(context.Background(), "hello")

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if _, ok := m["request_id"]; ok {
		t.Errorf("request_id present without an ID in context: %v", m["request_id"])
	}
}

// The stamping must survive a derived logger (slog.With), i.e. WithAttrs must
// re-wrap rather than unwrap back to a plain handler.
func TestContextHandler_SurvivesWith(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(&contextHandler{Handler: slog.NewJSONHandler(&buf, nil)}).With("component", "test")

	ctx := WithRequestID(context.Background(), "req-xyz")
	logger.InfoContext(ctx, "hello")

	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if m["request_id"] != "req-xyz" {
		t.Errorf("request_id = %v, want req-xyz (lost across With)", m["request_id"])
	}
	if m["component"] != "test" {
		t.Errorf("component = %v, want test", m["component"])
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	}
	for in, want := range tests {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
