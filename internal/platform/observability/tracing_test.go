package observability

import (
	"context"
	"testing"
)

// With no endpoint, tracing must stay disabled: InitTracer installs nothing,
// returns no error, and hands back a shutdown that is safe to call.
func TestInitTracer_DisabledWhenNoEndpoint(t *testing.T) {
	shutdown, err := InitTracer(context.Background(), TracingConfig{ServiceName: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown must never be nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown returned error: %v", err)
	}
}
