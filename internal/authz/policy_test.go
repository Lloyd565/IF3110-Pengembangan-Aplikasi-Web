package authz

import "testing"

func TestAllows(t *testing.T) {
	semua := []Action{SchemaView, DataView, MigrationView, MigrationCreate, MigrationUpdate, MigrationExecute}
	aksiTulis := map[Action]bool{MigrationCreate: true, MigrationUpdate: true, MigrationExecute: true}

	for _, act := range semua {
		if Allows(LevelNone, act) {
			t.Errorf("none seharusnya ditolak untuk %s", act)
		}
		if !Allows(LevelEdit, act) {
			t.Errorf("edit seharusnya boleh untuk %s", act)
		}
		if got, want := Allows(LevelView, act), !aksiTulis[act]; got != want {
			t.Errorf("view untuk %s: dapat %v, seharusnya %v", act, got, want)
		}
	}

	if Allows(LevelEdit, Action("aksi.tidak.dikenal")) {
		t.Error("aksi tidak dikenal harus ditolak")
	}
}