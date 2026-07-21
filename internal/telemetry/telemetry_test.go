package telemetry

import (
	"context"
	"testing"
)

func TestEmitIsBestEffort(t *testing.T) {
	Emit(context.Background(), nil, nil, Event{Name: "cli.test", Operation: "test"})
}
