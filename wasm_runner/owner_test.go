package main

import (
	"errors"
	"testing"
)

func newTestOwnerManager(t *testing.T, dir string) *ownerManager {
	t.Helper()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	om, err := newOwnerManager(st)
	if err != nil {
		t.Fatalf("newOwnerManager: %v", err)
	}
	return om
}

func TestOwnerRegisterTOFU(t *testing.T) {
	om := newTestOwnerManager(t, t.TempDir())

	if _, ok := om.key(); ok {
		t.Fatalf("fresh manager should have no owner key")
	}

	// 不正な鍵（base64 でない・長さ違い）は登録できない
	for _, bad := range []string{"", "???", "c2hvcnQ="} {
		if _, err := om.register(bad); err == nil {
			t.Fatalf("register(%q) should fail", bad)
		}
	}
	if _, ok := om.key(); ok {
		t.Fatalf("failed registration must not set the owner key")
	}

	k := genTestKey(t)
	rec, err := om.register(k.pubB64())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if rec.PublicKey != k.pubB64() || rec.RegisteredAt == "" {
		t.Fatalf("unexpected owner record: %+v", rec)
	}
	if got, ok := om.key(); !ok || got != k.pubB64() {
		t.Fatalf("key() = (%q, %v), want (%q, true)", got, ok, k.pubB64())
	}

	// TOFU: 2回目の登録は同じ鍵でも別の鍵でも拒否される
	if _, err := om.register(k.pubB64()); !errors.Is(err, errOwnerExists) {
		t.Fatalf("re-register same key: err = %v, want errOwnerExists", err)
	}
	if _, err := om.register(genTestKey(t).pubB64()); !errors.Is(err, errOwnerExists) {
		t.Fatalf("re-register other key: err = %v, want errOwnerExists", err)
	}
}

// TestOwnerPersistenceAcrossRestart は再起動後も登録済みオーナー鍵が保持され、
// 再登録が引き続き拒否されることを確認する
func TestOwnerPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	k := genTestKey(t)

	om1 := newTestOwnerManager(t, dir)
	if _, err := om1.register(k.pubB64()); err != nil {
		t.Fatalf("register: %v", err)
	}

	om2 := newTestOwnerManager(t, dir)
	if got, ok := om2.key(); !ok || got != k.pubB64() {
		t.Fatalf("key after restart = (%q, %v), want (%q, true)", got, ok, k.pubB64())
	}
	if _, err := om2.register(genTestKey(t).pubB64()); !errors.Is(err, errOwnerExists) {
		t.Fatalf("register after restart: err = %v, want errOwnerExists", err)
	}
}
