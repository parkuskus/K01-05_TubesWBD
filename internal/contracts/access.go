package contracts

type AccessLevel uint8

const (
	AccessNone AccessLevel = iota
	AccessViewOnly
	AccessViewEdit
)

type DatabaseAction uint8

const (
	ActionViewSchema DatabaseAction = iota + 1
	ActionViewData
	ActionViewMigrationHistory
	ActionCreateMigration
	ActionEditMigration
	ActionRunMigration
)

// EffectiveAccess returns the strongest group grant, or no access if any grant is invalid.
func EffectiveAccess(grants []AccessLevel) AccessLevel {
	effective := AccessNone
	for _, grant := range grants {
		if grant > AccessViewEdit {
			return AccessNone
		}
		if grant > effective {
			effective = grant
		}
	}
	return effective
}

// Allows reports whether the access level permits the database action.
func (level AccessLevel) Allows(action DatabaseAction) bool {
	if level > AccessViewEdit {
		return false
	}

	switch action {
	case ActionViewSchema, ActionViewData, ActionViewMigrationHistory:
		return level >= AccessViewOnly
	case ActionCreateMigration, ActionEditMigration, ActionRunMigration:
		return level >= AccessViewEdit
	default:
		return false
	}
}
