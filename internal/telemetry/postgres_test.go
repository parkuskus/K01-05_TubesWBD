package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/codes"
)

func TestQueryTracerRedactsParametersAndDatabaseErrors(t *testing.T) {
	logs, exporter := capture(t)
	tracer := &QueryTracer{DatabaseID: "12", Role: "target"}
	ctx := WithFields(context.Background(), Fields{ActorID: "7", JobID: "job-1", ExecutionID: "exec-1", Attempt: 1})
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL:  "UPDATE items SET value='private-literal' WHERE id=$1 /* private-comment */",
		Args: []any{pgx.QueryResultFormatsByOID{}, "private-parameter"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{Code: "23505", Message: "private-error", Detail: "private-detail"}})
	entries := records(t, logs)
	if entries[0]["parameter_count"] != float64(1) || entries[0]["database_id"] != "12" || entries[1]["sqlstate"] != "23505" || entries[1]["status"] != "FAILED" {
		t.Fatal("query metadata incomplete")
	}
	spans := exporter.GetSpans()
	data, _ := json.Marshal(spans)
	if strings.Contains(logs.String()+string(data), "private-") || spans[0].Status.Code != codes.Error {
		t.Fatal("sensitive query data leaked or error span missing")
	}
}

func TestQueryTracerDoesNotReportImplicitRollbackAsCommit(t *testing.T) {
	logs, exporter := capture(t)
	tracer := &QueryTracer{DatabaseID: "12", Role: "target"}
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "commit"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("ROLLBACK")})
	entry := records(t, logs)[1]
	if entry["event"] != "sql.transaction.commit.result" || entry["status"] != "FAILED" || exporter.GetSpans()[0].Status.Code != codes.Error {
		t.Fatal("implicit rollback reported as a successful commit")
	}
}

func TestPostgresTelemetryIntegration(t *testing.T) {
	dsn := os.Getenv("TELEMETRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TELEMETRY_TEST_DATABASE_URL not set")
	}
	logs, exporter := capture(t)
	db, err := OpenPlatform(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx = WithFields(ctx, Fields{RequestID: requestID(""), ActorID: "7", JobID: "job-1"})
	ctx, root := StartSpan(ctx, "test.postgres")
	defer root.End()
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE telemetry_values (id integer PRIMARY KEY, value text)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO telemetry_values VALUES (1, $1), (2, 'private-literal')`, "private-parameter"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT value FROM telemetry_values ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO telemetry_values VALUES (1, 'private-error-detail')`); err == nil {
		t.Fatal("expected duplicate-key error")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	rows, err = db.QueryContext(ctx, `SELECT 1 / (n - 2) FROM generate_series(1, 3) AS n`)
	if err == nil {
		for rows.Next() {
			var value int
			_ = rows.Scan(&value)
		}
		err = rows.Err()
		_ = rows.Close()
	}
	if SQLState(err) != "22012" {
		t.Fatal("expected row-stream division error")
	}
	entries := records(t, logs)
	events := make(map[string]int)
	var readRows, duplicateError, streamError bool
	for _, entry := range entries {
		events[entry["event"].(string)]++
		if entry["actor_id"] != "7" || entry["job_id"] != "job-1" || entry["trace_id"] != root.SpanContext().TraceID().String() {
			t.Fatal("database/sql lost correlation context")
		}
		readRows = readRows || entry["event"] == "sql.query.result" && entry["rows_affected"] == float64(2)
		duplicateError = duplicateError || entry["sqlstate"] == "23505"
		streamError = streamError || entry["sqlstate"] == "22012"
	}
	if events["sql.transaction.begin.result"] != 3 || events["sql.transaction.commit.result"] != 2 || events["sql.transaction.rollback.result"] != 1 || !readRows || !duplicateError || !streamError {
		t.Fatalf("query/transaction coverage incomplete: %v", events)
	}
	spanJSON, _ := json.Marshal(exporter.GetSpans())
	if strings.Contains(logs.String()+string(spanJSON), "private-") {
		t.Fatal("PostgreSQL values or errors leaked into logs/spans")
	}
	t.Logf("log sample: %s", strings.Split(logs.String(), "\n")[0])
	t.Logf("span sample: name=%s trace_id=%s parent_span_id=%s", exporter.GetSpans()[0].Name,
		exporter.GetSpans()[0].SpanContext.TraceID(), exporter.GetSpans()[0].Parent.SpanID())
}
