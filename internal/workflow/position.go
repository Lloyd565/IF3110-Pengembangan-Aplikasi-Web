package workflow

import (
	"fmt"
	"slices"
)

type attempt struct {
	ID          string
	MigrationID int64
	Action      string
	Status      string
	Reverts     string
}

var unresolved = map[string]bool{"commit_unknown": true, "partial": true, "partially_reverted": true}

type position struct {
	active  []int64
	undone  []int64
	applied map[int64]string
	rolled  map[int64]string
	blocked string
	// blockedID/blockedStatus: execution yang menahan database. Rollback yang menargetkan execution
	// partial ini adalah kompensasi untuk memulihkannya, jadi tidak ikut ditahan.
	blockedID, blockedStatus string
}

func replay(history []attempt) position {
	p := position{applied: map[int64]string{}, rolled: map[int64]string{}}
	pending := map[int64]attempt{}
	for _, a := range history {
		if unresolved[a.Status] {
			pending[a.MigrationID] = a
		}
		if a.Status != "success" {
			continue
		}
		delete(pending, a.MigrationID)
		switch a.Action {
		case "apply":
			p.active = append(remove(p.active, a.MigrationID), a.MigrationID)
			p.undone = nil
			p.applied[a.MigrationID] = a.ID
		case "rollback":
			p.active = remove(p.active, a.MigrationID)
			p.undone = append(p.undone, a.MigrationID)
			p.rolled[a.MigrationID] = a.ID
			delete(p.applied, a.MigrationID)
		case "forward":
			p.undone = remove(p.undone, a.MigrationID)
			p.active = append(remove(p.active, a.MigrationID), a.MigrationID)
			p.applied[a.MigrationID] = a.ID
			delete(p.rolled, a.MigrationID)
		}
	}
	for _, a := range history {
		if l := pending[a.MigrationID]; l.ID == a.ID && unresolved[a.Status] {
			p.blocked = fmt.Sprintf("Execution %s berstatus %s; rekonsiliasi dulu sebelum perubahan lain dijalankan.", a.ID, a.Status)
			p.blockedID, p.blockedStatus = a.ID, a.Status
		}
	}
	return p
}

func remove(stack []int64, id int64) []int64 {
	out := stack[:0:0]
	for _, x := range stack {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func top(stack []int64) int64 {
	if len(stack) == 0 {
		return 0
	}
	return stack[len(stack)-1]
}

func (p position) check(action string, migrationID int64, target string) string {
	if action == "rollback" && p.blockedID != "" && p.blockedID == target && (p.blockedStatus == "partial" || p.blockedStatus == "partially_reverted" || p.blockedStatus == "commit_unknown") {
		return ""
	}
	if p.blocked != "" {
		return p.blocked
	}
	switch action {
	case "apply":
		if slices.Contains(p.active, migrationID) {
			return "Migration sudah aktif. Gunakan rollback atau buat migration baru."
		}
		if slices.Contains(p.undone, migrationID) {
			return "Migration ini sudah di-rollback; gunakan forward untuk menerapkannya kembali."
		}
	case "rollback":
		if top(p.active) != migrationID {
			return "Rollback hanya untuk migration aktif terakhir."
		}
		if p.applied[migrationID] != target {
			return "Execution target bukan execution yang membuat migration ini aktif."
		}
	case "forward":
		if top(p.undone) != migrationID {
			return "Forward hanya untuk migration yang terakhir di-rollback, sesuai urutan linear."
		}
		if p.rolled[migrationID] != target {
			return "Execution target bukan rollback terakhir migration ini."
		}
	default:
		return "Aksi tidak dikenal."
	}
	return ""
}

func hideRollback[T any](list []T, get func(T) attempt) []T {
	hidden := map[string]bool{}
	for _, x := range list {
		if a := get(x); a.Action == "rollback" && a.Status == "success" {
			hidden[a.ID], hidden[a.Reverts] = true, true
		}
	}
	out := []T{}
	for _, x := range list {
		if !hidden[get(x).ID] {
			out = append(out, x)
		}
	}
	return out
}
