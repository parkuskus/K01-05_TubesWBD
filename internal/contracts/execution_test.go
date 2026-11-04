package contracts

import (
	"errors"
	"testing"
)

func validNormalPlan() ExecutionPlan {
	plan := ExecutionPlan{
		RequestID:                "req-1",
		ExecutionID:              "exec-1",
		MigrationID:              "mig-1",
		RevisionID:               "rev-1",
		Version:                  1,
		Direction:                DirectionApply,
		Mode:                     ModeNormal,
		ActorID:                  "user-editor",
		TargetDatabaseID:         "db-target-1",
		TargetConnectionRevision: 1,
		ExpectedActiveHead:       "",
		PolicyVersion:            1,
		UpSQL:                    "CREATE TABLE items (id BIGINT PRIMARY KEY);",
		DownSQL:                  "DROP TABLE items;",
	}
	plan.PlanChecksum = plan.ComputeChecksum()
	return plan
}

func TestExecutionEnumsValidation(t *testing.T) {
	validDirections := []OperationDirection{
		DirectionApply,
		DirectionRollback,
		DirectionForward,
		DirectionResume,
		DirectionCompensate,
	}
	for _, dir := range validDirections {
		if !dir.IsValid() {
			t.Errorf("OperationDirection(%q).IsValid() = false, want true", dir)
		}
	}
	if OperationDirection("INVALID").IsValid() {
		t.Error("expected INVALID OperationDirection to fail validation")
	}

	if !ModeNormal.IsValid() || !ModeBatch.IsValid() || ExecutionMode("STREAM").IsValid() {
		t.Error("unexpected ExecutionMode validation result")
	}

	if !GateReasonPartial.IsValid() || !GateReasonPartiallyReverted.IsValid() || !GateReasonUnknown.IsValid() || GateReason("NONE").IsValid() {
		t.Error("unexpected GateReason validation result")
	}

	if !JobStatusPending.IsValid() || !JobStatusPartial.IsValid() || !JobStatusSuccess.IsValid() || JobStatus("DONE").IsValid() {
		t.Error("unexpected JobStatus validation result")
	}
	if !JobStatusSuccess.IsTerminal() || !JobStatusFailed.IsTerminal() || JobStatusPartial.IsTerminal() || JobStatusRunning.IsTerminal() {
		t.Error("unexpected JobStatus.IsTerminal() result")
	}

	if !AttemptStatusStarted.IsValid() || !AttemptStatusCommitted.IsValid() || AttemptStatus("RETRY").IsValid() {
		t.Error("unexpected AttemptStatus validation result")
	}
}

func TestExecutionPlanValidationAndSelectedSQL(t *testing.T) {
	base := validNormalPlan()
	if err := base.Validate(); err != nil {
		t.Fatalf("validNormalPlan().Validate() error = %v, want nil", err)
	}
	sqlText, err := base.SelectedSQL()
	if err != nil || sqlText != base.UpSQL {
		t.Fatalf("SelectedSQL() = (%q, %v), want (%q, nil)", sqlText, err, base.UpSQL)
	}

	rollbackPlan := base
	rollbackPlan.Direction = DirectionRollback
	rollbackPlan.OriginExecutionID = "exec-0"
	rollbackPlan.PlanChecksum = rollbackPlan.ComputeChecksum()
	sqlText, err = rollbackPlan.SelectedSQL()
	if err != nil || sqlText != rollbackPlan.DownSQL {
		t.Fatalf("rollback SelectedSQL() = (%q, %v), want (%q, nil)", sqlText, err, rollbackPlan.DownSQL)
	}

	t.Run("irreversible migration valid on apply but rejected on rollback", func(t *testing.T) {
		irrev := base
		irrev.DownSQL = ""
		irrev.IsIrreversible = true
		irrev.IrreversibleReason = "menghapus kolom legacy secara permanen"
		irrev.PlanChecksum = irrev.ComputeChecksum()
		if err := irrev.Validate(); err != nil {
			t.Fatalf("irreversible apply plan Validate() error = %v, want nil", err)
		}

		irrev.Direction = DirectionRollback
		irrev.OriginExecutionID = "exec-1"
		irrev.PlanChecksum = irrev.ComputeChecksum()
		if err := irrev.Validate(); !errors.Is(err, ErrIrreversibleMigration) {
			t.Fatalf("irreversible rollback Validate() error = %v, want ErrIrreversibleMigration", err)
		}
	})

	t.Run("irreversible XOR down SQL enforced", func(t *testing.T) {
		both := base
		both.IsIrreversible = true
		both.IrreversibleReason = "ada alasan tapi masih ada down sql"
		both.PlanChecksum = both.ComputeChecksum()
		if err := both.Validate(); !errors.Is(err, ErrInvalidExecutionPlan) {
			t.Fatalf("plan with both down SQL and irreversible flag = %v, want ErrInvalidExecutionPlan", err)
		}

		neither := base
		neither.DownSQL = ""
		neither.IsIrreversible = false
		neither.PlanChecksum = neither.ComputeChecksum()
		if err := neither.Validate(); !errors.Is(err, ErrInvalidExecutionPlan) {
			t.Fatalf("reversible plan without down SQL = %v, want ErrInvalidExecutionPlan", err)
		}
	})

	t.Run("rollback and resume require origin execution ID", func(t *testing.T) {
		missingOrigin := base
		missingOrigin.Direction = DirectionRollback
		missingOrigin.OriginExecutionID = ""
		missingOrigin.PlanChecksum = missingOrigin.ComputeChecksum()
		if err := missingOrigin.Validate(); !errors.Is(err, ErrInvalidExecutionPlan) {
			t.Fatalf("rollback without origin = %v, want ErrInvalidExecutionPlan", err)
		}
	})

	t.Run("batch mode requires valid BatchConfig", func(t *testing.T) {
		batchPlan := base
		batchPlan.Mode = ModeBatch
		batchPlan.PlanChecksum = batchPlan.ComputeChecksum()
		if err := batchPlan.Validate(); !errors.Is(err, ErrInvalidExecutionPlan) {
			t.Fatalf("batch plan with empty BatchConfig = %v, want ErrInvalidExecutionPlan", err)
		}

		batchPlan.Batch = BatchConfig{
			TableSchema:      "public",
			TableName:        "orders",
			PrimaryKeyColumn: "id",
			ScopeWhere:       "updated_flag = FALSE",
			BatchSize:        10000,
		}
		batchPlan.PlanChecksum = batchPlan.ComputeChecksum()
		if err := batchPlan.Validate(); err != nil {
			t.Fatalf("valid batch plan Validate() error = %v, want nil", err)
		}
	})

	t.Run("tampered checksum fails validation", func(t *testing.T) {
		tampered := base
		tampered.UpSQL = "DROP TABLE items;"
		if err := tampered.Validate(); !errors.Is(err, ErrInvalidExecutionPlan) {
			t.Fatalf("tampered plan Validate() = %v, want ErrInvalidExecutionPlan", err)
		}
	})
}

