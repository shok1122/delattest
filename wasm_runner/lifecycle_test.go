package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
)

func newTestManager(t *testing.T, dir string) *lifecycleManager {
	t.Helper()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	lm, err := newLifecycleManager(st, newProver())
	if err != nil {
		t.Fatalf("newLifecycleManager: %v", err)
	}
	return lm
}

func TestRegisterAndStatus(t *testing.T) {
	lm := newTestManager(t, t.TempDir())

	rec, err := lm.register([]byte("hello data"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if rec.State != stateRegistered {
		t.Fatalf("state = %s, want REGISTERED", rec.State)
	}

	info, err := lm.status(rec.DataID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if info.State != stateRegistered {
		t.Fatalf("status state = %s, want REGISTERED", info.State)
	}

	if _, err := lm.status("d-0000000000000000"); !errors.Is(err, errNotFound) {
		t.Fatalf("status unknown id: err = %v, want errNotFound", err)
	}
}

func TestExecuteLifecycle(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	data := []byte("secret-input")
	rec, _ := lm.register(data, "")

	// REGISTERED -> IN_USE。復号済みデータが返る
	input, err := lm.beginExecute(rec.DataID, "")
	if err != nil {
		t.Fatalf("beginExecute: %v", err)
	}
	if !bytes.Equal(input, data) {
		t.Fatalf("input = %q, want %q", input, data)
	}

	// IN_USE 中は execute/delete とも排他される（§5 不変条件2）
	if _, err := lm.beginExecute(rec.DataID, ""); !errors.Is(err, errBusy) {
		t.Fatalf("concurrent execute: err = %v, want errBusy", err)
	}
	if _, err := lm.delete(rec.DataID, ""); !errors.Is(err, errBusy) {
		t.Fatalf("delete while IN_USE: err = %v, want errBusy", err)
	}

	// execute complete -> REGISTERED に戻り、再び操作可能
	lm.endExecute(rec.DataID)
	if info, _ := lm.status(rec.DataID); info.State != stateRegistered {
		t.Fatalf("state after endExecute = %s, want REGISTERED", info.State)
	}
	if _, err := lm.delete(rec.DataID, ""); err != nil {
		t.Fatalf("delete after endExecute: %v", err)
	}
}

func TestDeleteIssuesCertificateAndBecomesTerminal(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	rec, _ := lm.register([]byte("to be deleted"), "")

	cert, err := lm.delete(rec.DataID, "")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	var c deletionCertificate
	if err := json.Unmarshal(cert, &c); err != nil {
		t.Fatalf("certificate is not valid JSON: %v", err)
	}
	if c.DataID != rec.DataID || c.ContentHash != rec.ContentHash || c.DeletedAt == "" {
		t.Fatalf("certificate fields mismatch: %+v", c)
	}

	// DELETED は終端状態（§5 不変条件3）: execute は不可、再削除も不可、proof は取得可
	if _, err := lm.beginExecute(rec.DataID, ""); !errors.Is(err, errDeleted) {
		t.Fatalf("execute after delete: err = %v, want errDeleted", err)
	}
	if _, err := lm.delete(rec.DataID, ""); !errors.Is(err, errDeleted) {
		t.Fatalf("double delete: err = %v, want errDeleted", err)
	}
	proof, err := lm.proof(rec.DataID)
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if !bytes.Equal(proof, cert) {
		t.Fatalf("proof differs from the certificate issued at deletion")
	}

	// 削除前のデータには proof は存在しない
	rec2, _ := lm.register([]byte("alive"), "")
	if _, err := lm.proof(rec2.DataID); !errors.Is(err, errNotDeleted) {
		t.Fatalf("proof on alive data: err = %v, want errNotDeleted", err)
	}
}

func TestOwnerTokenEnforcement(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	rec, _ := lm.register([]byte("private"), "owner-secret")

	if _, err := lm.beginExecute(rec.DataID, ""); !errors.Is(err, errForbidden) {
		t.Fatalf("execute without token: err = %v, want errForbidden", err)
	}
	if _, err := lm.delete(rec.DataID, "wrong"); !errors.Is(err, errForbidden) {
		t.Fatalf("delete with wrong token: err = %v, want errForbidden", err)
	}
	if _, err := lm.beginExecute(rec.DataID, "owner-secret"); err != nil {
		t.Fatalf("execute with correct token: %v", err)
	}
	lm.endExecute(rec.DataID)
	if _, err := lm.delete(rec.DataID, "owner-secret"); err != nil {
		t.Fatalf("delete with correct token: %v", err)
	}
}

// TestConcurrentDeleteRace は同一データIDへ並行に削除をかけても、
// ちょうど1回だけ成功し他は既削除エラーになることを確認する（TOCTOU対策 §5）
func TestConcurrentDeleteRace(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	rec, _ := lm.register([]byte("contended"), "")

	const n = 16
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = lm.delete(rec.DataID, "")
		}()
	}
	wg.Wait()

	success := 0
	for _, err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, errDeleted) || errors.Is(err, errBusy):
			// ok
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("delete succeeded %d times, want exactly 1", success)
	}
	if info, _ := lm.status(rec.DataID); info.State != stateDeleted {
		t.Fatalf("final state = %s, want DELETED", info.State)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	lm1 := newTestManager(t, dir)
	alive, _ := lm1.register([]byte("survives restart"), "tok")
	deleted, _ := lm1.register([]byte("deleted before restart"), "")
	cert, err := lm1.delete(deleted.DataID, "")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	// 再起動を模擬: 同じディレクトリから新しいマネージャを作る
	lm2 := newTestManager(t, dir)

	info, err := lm2.status(alive.DataID)
	if err != nil || info.State != stateRegistered {
		t.Fatalf("alive data after restart: info=%+v err=%v", info, err)
	}
	// オーナートークンも復元されている
	if _, err := lm2.beginExecute(alive.DataID, "bad"); !errors.Is(err, errForbidden) {
		t.Fatalf("token check after restart: err = %v, want errForbidden", err)
	}
	input, err := lm2.beginExecute(alive.DataID, "tok")
	if err != nil {
		t.Fatalf("execute after restart: %v", err)
	}
	if string(input) != "survives restart" {
		t.Fatalf("input after restart = %q", input)
	}
	lm2.endExecute(alive.DataID)

	// 削除済みデータは DELETED のまま、証明も同一のものが再取得できる
	proof, err := lm2.proof(deleted.DataID)
	if err != nil {
		t.Fatalf("proof after restart: %v", err)
	}
	if !bytes.Equal(proof, cert) {
		t.Fatalf("proof after restart differs from original certificate")
	}
}

// TestCryptoShreddingOnDisk は削除後、ディスク上に blob も DEK も残らないことを確認する（§9.1）
func TestCryptoShreddingOnDisk(t *testing.T) {
	dir := t.TempDir()
	lm := newTestManager(t, dir)
	rec, _ := lm.register([]byte("shred me"), "")

	// 削除前: blob が存在し、meta に dek が含まれる
	if _, err := os.Stat(lm.store.blobPath(rec.DataID)); err != nil {
		t.Fatalf("blob missing before delete: %v", err)
	}
	metaRaw, _ := os.ReadFile(lm.store.metaPath(rec.DataID))
	if !bytes.Contains(metaRaw, []byte(`"dek"`)) {
		t.Fatalf("meta before delete should contain dek: %s", metaRaw)
	}

	if _, err := lm.delete(rec.DataID, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// 削除後: blob は消え、meta から dek が破棄されている
	if _, err := os.Stat(lm.store.blobPath(rec.DataID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob still exists after delete: err=%v", err)
	}
	metaRaw, _ = os.ReadFile(lm.store.metaPath(rec.DataID))
	if bytes.Contains(metaRaw, []byte(`"dek"`)) {
		t.Fatalf("meta after delete still contains dek: %s", metaRaw)
	}
}

// TestCrashDuringDeleteRecovery は削除の途中（DELETING 永続化後）でクラッシュしても、
// 次回起動時に削除が完遂され証明が発行されることを確認する
func TestCrashDuringDeleteRecovery(t *testing.T) {
	dir := t.TempDir()
	lm := newTestManager(t, dir)
	rec, _ := lm.register([]byte("crash victim"), "")

	// クラッシュを模擬: DELETING（DEK破棄済み）を永続化した直後の状態を作り、
	// finishDelete を呼ばずに新しいマネージャを起動する
	lm.mu.Lock()
	rec.State = stateDeleting
	rec.DEK = nil
	if err := lm.store.writeMeta(rec); err != nil {
		lm.mu.Unlock()
		t.Fatalf("writeMeta: %v", err)
	}
	lm.mu.Unlock()

	lm2 := newTestManager(t, dir)
	info, err := lm2.status(rec.DataID)
	if err != nil {
		t.Fatalf("status after recovery: %v", err)
	}
	if info.State != stateDeleted {
		t.Fatalf("state after recovery = %s, want DELETED", info.State)
	}
	if _, err := lm2.proof(rec.DataID); err != nil {
		t.Fatalf("proof after recovery: %v", err)
	}
	if _, err := os.Stat(lm2.store.blobPath(rec.DataID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob still exists after recovery: err=%v", err)
	}
}

func TestSealOpenRoundtrip(t *testing.T) {
	dek := bytes.Repeat([]byte{0x42}, 32)
	plain := []byte("plaintext payload")

	sealed, err := sealBytes(dek, plain)
	if err != nil {
		t.Fatalf("sealBytes: %v", err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatalf("sealed blob contains plaintext")
	}
	got, err := openBytes(dek, sealed)
	if err != nil {
		t.Fatalf("openBytes: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip mismatch: %q", got)
	}

	// 別の鍵では復号できない（＝鍵破棄で復元不能になることの根拠）
	otherKey := bytes.Repeat([]byte{0x43}, 32)
	if _, err := openBytes(otherKey, sealed); err == nil {
		t.Fatalf("openBytes with wrong key should fail")
	}
	// 改ざん検知
	sealed[len(sealed)-1] ^= 0xff
	if _, err := openBytes(dek, sealed); err == nil {
		t.Fatalf("openBytes with tampered blob should fail")
	}
}
