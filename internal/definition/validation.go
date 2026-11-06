package definition

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/execution"
)

// shortcut: cap batches at the documented initial size; raise only with benchmark evidence and a coordinated executor limit.
const maxBatchSize = 10_000

type ValidationIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ValidationReport struct {
	MigrationID        string            `json:"migration_id,omitempty"`
	Revision           int               `json:"revision,omitempty"`
	Checksum           string            `json:"checksum,omitempty"`
	Valid              bool              `json:"valid"`
	Errors             []ValidationIssue `json:"errors"`
	Warnings           []ValidationIssue `json:"warnings"`
	CompatibilityNotes string            `json:"compatibility_notes"`
}

type batchPlanInput struct {
	TableSchema      string `json:"table_schema"`
	TableName        string `json:"table_name"`
	PrimaryKeyColumn string `json:"primary_key_column"`
	ScopeWhere       string `json:"scope_where"`
	BatchSize        int    `json:"batch_size"`
}

// ValidateInput performs static checks only; it never connects to a target or executes SQL.
func ValidateInput(in Input) ValidationReport {
	report := ValidationReport{
		Errors: []ValidationIssue{},
		Warnings: []ValidationIssue{
			{Code: "COMPATIBILITY_UNVERIFIED", Message: "Application compatibility is not checked; approvers must review the stored notes."},
			{Code: "SQL_RISK_REQUIRES_REVIEW", Message: "Review both SQL directions and their data impact; static checks do not parse all PostgreSQL operations."},
			{Code: "EXECUTION_PRECHECK_PENDING", Message: "Approval, policy version, active head, target lock, and unresolved execution gates are checked by later workflow stages."},
		},
		CompatibilityNotes: in.Notes,
	}
	addError := func(code, message string) {
		report.Errors = append(report.Errors, ValidationIssue{Code: code, Message: message})
	}
	if err := in.Validate(); err != nil {
		addError("INVALID_INPUT", "Migration fields or irreversible/down SQL combination are invalid.")
		report.Valid = false
		return report
	}
	if in.Irreversible {
		report.Warnings = append(report.Warnings, ValidationIssue{Code: "IRREVERSIBLE", Message: "This migration has no down SQL; approvers must review its irreversible reason."})
	}
	if err := execution.ValidateUserSQL(in.UpSQL); err != nil {
		addError("UNSAFE_SQL", "Up SQL contains a prohibited transaction control statement or internal schema reference.")
	}
	if !in.Irreversible {
		if err := execution.ValidateUserSQL(in.DownSQL); err != nil {
			addError("UNSAFE_SQL", "Down SQL contains a prohibited transaction control statement or internal schema reference.")
		}
	}
	if in.Mode == "BATCH" {
		cfg, err := decodeBatchConfig(in.BatchPlan)
		if err != nil || cfg.Validate() != nil || cfg.BatchSize > maxBatchSize {
			addError("INVALID_BATCH_CONFIG", "Batch plan must define valid identifiers and a batch size between 1 and 10000.")
		} else {
			invalidIdentifier := false
			for _, identifier := range []string{cfg.TableSchema, cfg.TableName, cfg.PrimaryKeyColumn} {
				if _, err := execution.QuoteIdentifier(identifier); err != nil {
					invalidIdentifier = true
					break
				}
			}
			if invalidIdentifier {
				addError("INVALID_BATCH_CONFIG", "Batch identifiers are not supported.")
			} else if err := execution.ValidateScopeWhere(cfg.ScopeWhere); err != nil {
				addError("INVALID_BATCH_SCOPE", "Batch scope must be a single safe WHERE predicate.")
			} else if _, err := execution.BuildScopedBatchUpdateSQL(in.UpSQL, cfg); err != nil {
				addError("INVALID_BATCH_SQL", "Batch up SQL must be a supported single UPDATE statement.")
			} else if !in.Irreversible {
				if _, err := execution.BuildBatchCompensateSQL(in.DownSQL, cfg); err != nil {
					addError("INVALID_BATCH_SQL", "Batch down SQL must be a supported compensation UPDATE statement.")
				}
			}
		}
	}
	report.Valid = len(report.Errors) == 0
	return report
}

func decodeBatchConfig(raw json.RawMessage) (contracts.BatchConfig, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return contracts.BatchConfig{}, errors.New("batch plan must be an object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return contracts.BatchConfig{}, err
		}
		key, ok := token.(string)
		if !ok {
			return contracts.BatchConfig{}, errors.New("invalid batch plan key")
		}
		if _, exists := seen[key]; exists {
			return contracts.BatchConfig{}, errors.New("duplicate batch plan key")
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return contracts.BatchConfig{}, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return contracts.BatchConfig{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return contracts.BatchConfig{}, errors.New("multiple JSON values")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input batchPlanInput
	if err := decoder.Decode(&input); err != nil {
		return contracts.BatchConfig{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return contracts.BatchConfig{}, errors.New("multiple JSON values")
	}
	return contracts.BatchConfig{
		TableSchema:      input.TableSchema,
		TableName:        input.TableName,
		PrimaryKeyColumn: input.PrimaryKeyColumn,
		ScopeWhere:       input.ScopeWhere,
		BatchSize:        input.BatchSize,
	}, nil
}
