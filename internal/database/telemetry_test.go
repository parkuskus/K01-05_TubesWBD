package database

import (
	"testing"
	"time"

	"if3110.local/db-platform/internal/telemetry"
)

func TestViewerAndExecutorConfigsUseTargetTelemetry(t *testing.T) {
	connection := Connection{ID: "12", Host: "localhost", Port: 5432, Database: "target", Username: "test", SSLMode: "disable"}
	viewer, err := targetConfig(connection, "private-password")
	if err != nil {
		t.Fatal(err)
	}
	executor, err := executorTargetConfig(connection, "private-password", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []struct{ tracer any }{{viewer.Tracer}, {executor.Tracer}} {
		tracer, ok := cfg.tracer.(*telemetry.QueryTracer)
		if !ok || tracer.DatabaseID != "12" || tracer.Role != "target" {
			t.Fatal("viewer or pinned executor connection is missing target telemetry")
		}
	}
}
