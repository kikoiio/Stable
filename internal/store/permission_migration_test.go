package store

import (
	"path/filepath"
	"testing"
)

func TestPermissionAuthorityMigrationFromV7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`ALTER TABLE approval_requests DROP COLUMN authority_json; ALTER TABLE approval_requests DROP COLUMN reason; PRAGMA user_version=7`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, column := range []string{"authority_json", "reason"} {
		if !hasColumn(s.DB(), "approval_requests", column) {
			t.Fatalf("migration did not restore %s", column)
		}
	}
	var version int
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestPermissionDecisionUserMigrationFromV9(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`ALTER TABLE permission_decisions DROP COLUMN user_id; PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !hasColumn(s.DB(), "permission_decisions", "user_id") {
		t.Fatal("v10 migration did not restore permission decision user identity")
	}
	var version int
	if err = s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
