package contracts

import "testing"

func TestEffectiveAccess(t *testing.T) {
	tests := []struct {
		name   string
		grants []AccessLevel
		want   AccessLevel
	}{
		{name: "no grants", want: AccessNone},
		{name: "view only", grants: []AccessLevel{AccessViewOnly}, want: AccessViewOnly},
		{name: "edit grant wins", grants: []AccessLevel{AccessViewOnly, AccessViewEdit}, want: AccessViewEdit},
		{name: "grant order does not matter", grants: []AccessLevel{AccessViewEdit, AccessViewOnly}, want: AccessViewEdit},
		{name: "unknown grant fails closed", grants: []AccessLevel{AccessViewOnly, AccessLevel(255)}, want: AccessNone},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EffectiveAccess(test.grants); got != test.want {
				t.Errorf("EffectiveAccess(%v) = %v, want %v", test.grants, got, test.want)
			}
		})
	}
}

func TestAccessLevelAllows(t *testing.T) {
	tests := []struct {
		name   string
		level  AccessLevel
		action DatabaseAction
		want   bool
	}{
		{name: "view only can view schema", level: AccessViewOnly, action: ActionViewSchema, want: true},
		{name: "view only can view data", level: AccessViewOnly, action: ActionViewData, want: true},
		{name: "view only can view migration history", level: AccessViewOnly, action: ActionViewMigrationHistory, want: true},
		{name: "view only cannot run migration", level: AccessViewOnly, action: ActionRunMigration, want: false},
		{name: "edit can run migration", level: AccessViewEdit, action: ActionRunMigration, want: true},
		{name: "no access cannot view", level: AccessNone, action: ActionViewSchema, want: false},
		{name: "unknown level fails closed", level: AccessLevel(255), action: ActionViewSchema, want: false},
		{name: "unknown action fails closed", level: AccessViewEdit, action: DatabaseAction(255), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.level.Allows(test.action); got != test.want {
				t.Errorf("AccessLevel(%d).Allows(%d) = %t, want %t", test.level, test.action, got, test.want)
			}
		})
	}
}
