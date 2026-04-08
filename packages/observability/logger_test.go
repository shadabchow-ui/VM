package observability_test

import (
	"context"
	"testing"

	"github.com/compute-platform/compute-platform/packages/observability"
)

func TestNew_ReturnsLogger(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error", "invalid"} {
		log := observability.New(level)
		if log == nil {
			t.Errorf("New(%q) returned nil", level)
		}
	}
}

func TestFromContext_Empty(t *testing.T) {
	attrs := observability.FromContext(context.Background())
	if len(attrs) != 0 {
		t.Errorf("expected 0 attrs from empty context, got %d", len(attrs))
	}
}

func TestFromContext_WithValues(t *testing.T) {
	ctx := context.Background()
	ctx = observability.WithRequestID(ctx, "req-123")
	ctx = observability.WithHostID(ctx, "host-abc")
	ctx = observability.WithInstanceID(ctx, "inst_xyz")

	attrs := observability.FromContext(ctx)
	if len(attrs) != 3 {
		t.Errorf("expected 3 attrs, got %d", len(attrs))
	}
}
