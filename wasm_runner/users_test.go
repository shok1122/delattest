package main

import (
	"errors"
	"strings"
	"testing"
)

func newTestUserManager(t *testing.T, dir string) *userManager {
	t.Helper()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	um, err := newUserManager(st)
	if err != nil {
		t.Fatalf("newUserManager: %v", err)
	}
	return um
}

func TestUserCreateAndResolve(t *testing.T) {
	um := newTestUserManager(t, t.TempDir())

	rec, key, err := um.createUser()
	if err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if !strings.HasPrefix(rec.OwnerID, "u-") || !strings.HasPrefix(key, "ak-") || rec.CreatedAt == "" {
		t.Fatalf("unexpected user: rec=%+v key=%q", rec, key)
	}
	// レコードにはキーのハッシュのみが保存され、平文は含まれない
	if rec.APIKeyHash == "" || strings.Contains(rec.APIKeyHash, key) {
		t.Fatalf("api key must be stored as hash only: %+v", rec)
	}

	owner, err := um.resolveOwner(key)
	if err != nil || owner != rec.OwnerID {
		t.Fatalf("resolveOwner = (%q, %v), want (%q, nil)", owner, err, rec.OwnerID)
	}

	// キー未提示・無効なキーは errUnauthorized
	if _, err := um.resolveOwner(""); !errors.Is(err, errUnauthorized) {
		t.Fatalf("resolveOwner empty: err = %v, want errUnauthorized", err)
	}
	if _, err := um.resolveOwner("ak-wrong"); !errors.Is(err, errUnauthorized) {
		t.Fatalf("resolveOwner wrong: err = %v, want errUnauthorized", err)
	}

	// ユーザごとに owner_id・キーは異なる
	rec2, key2, err := um.createUser()
	if err != nil {
		t.Fatalf("createUser 2nd: %v", err)
	}
	if rec2.OwnerID == rec.OwnerID || key2 == key {
		t.Fatalf("users must be distinct: %+v %+v", rec, rec2)
	}
}

// TestUserPersistenceAcrossRestart は再起動後もユーザ表（owner_id ↔ キーハッシュ）が
// 保持されることを確認する
func TestUserPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	um1 := newTestUserManager(t, dir)
	rec, key, err := um1.createUser()
	if err != nil {
		t.Fatalf("createUser: %v", err)
	}

	um2 := newTestUserManager(t, dir)
	owner, err := um2.resolveOwner(key)
	if err != nil || owner != rec.OwnerID {
		t.Fatalf("resolveOwner after restart = (%q, %v), want (%q, nil)", owner, err, rec.OwnerID)
	}
	if _, err := um2.resolveOwner("ak-wrong"); !errors.Is(err, errUnauthorized) {
		t.Fatalf("resolveOwner wrong after restart: err = %v, want errUnauthorized", err)
	}
}
