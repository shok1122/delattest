package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// lifecycleManager 層のテストで使う識別子。署名検証はハンドラ層の責務なので、
// この層では検証済みのアップローダ公開鍵（base64 文字列）を直接渡す。
// この層では形式検証をしないため、値そのものは任意の文字列でよい
const (
	testUploader = "dGVzdC11cGxvYWRlci1wdWJsaWMta2V5LTAwMDA="
	testProgram  = "p-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testProgram2 = "p-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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

// regTest は testUploader が testProgram を許可した状態でデータを登録する
func regTest(t *testing.T, lm *lifecycleManager, data []byte) *metaRecord {
	t.Helper()
	rec, err := lm.register(data, testUploader, []string{testProgram})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return rec
}

func TestRegisterAndStatus(t *testing.T) {
	lm := newTestManager(t, t.TempDir())

	rec := regTest(t, lm, []byte("hello data"))
	if rec.State != stateRegistered {
		t.Fatalf("state = %s, want REGISTERED", rec.State)
	}
	if rec.Uploader != testUploader {
		t.Fatalf("uploader = %s, want %s", rec.Uploader, testUploader)
	}
	if len(rec.AllowedPrograms) != 1 || rec.AllowedPrograms[0] != testProgram {
		t.Fatalf("allowed_programs = %v, want [%s]", rec.AllowedPrograms, testProgram)
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

	// アップローダ鍵無しの登録は認められない（全データは署名検証済みの
	// アップローダに帰属する）
	if _, err := lm.register([]byte("orphan"), "", nil); err == nil {
		t.Fatalf("register without uploader should fail")
	}

	// ホワイトリスト省略（nil）は空リスト＝すべて拒否（deny by default）
	rec2, err := lm.register([]byte("deny by default"), testUploader, nil)
	if err != nil {
		t.Fatalf("register with nil whitelist: %v", err)
	}
	if rec2.AllowedPrograms == nil || len(rec2.AllowedPrograms) != 0 {
		t.Fatalf("allowed_programs = %#v, want empty non-nil slice", rec2.AllowedPrograms)
	}
	if _, err := lm.beginExecute([]string{rec2.DataID}, testProgram); !errors.Is(err, errProgramNotAllowed) {
		t.Fatalf("execute against empty whitelist: err = %v, want errProgramNotAllowed", err)
	}
}

func TestExecuteLifecycle(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	data := []byte("secret-input")
	rec := regTest(t, lm, data)

	// REGISTERED -> IN_USE。復号済みデータが返る
	inputs, err := lm.beginExecute([]string{rec.DataID}, testProgram)
	if err != nil {
		t.Fatalf("beginExecute: %v", err)
	}
	if len(inputs) != 1 || !bytes.Equal(inputs[0], data) {
		t.Fatalf("inputs = %q, want [%q]", inputs, data)
	}

	// IN_USE 中は execute/delete とも排他される（§5 不変条件2）
	if _, err := lm.beginExecute([]string{rec.DataID}, testProgram); !errors.Is(err, errBusy) {
		t.Fatalf("concurrent execute: err = %v, want errBusy", err)
	}
	if _, err := lm.delete(rec.DataID, testUploader, false); !errors.Is(err, errBusy) {
		t.Fatalf("delete while IN_USE: err = %v, want errBusy", err)
	}

	// execute complete -> REGISTERED に戻り、再び操作可能
	lm.endExecute([]string{rec.DataID})
	if info, _ := lm.status(rec.DataID); info.State != stateRegistered {
		t.Fatalf("state after endExecute = %s, want REGISTERED", info.State)
	}
	if _, err := lm.delete(rec.DataID, testUploader, false); err != nil {
		t.Fatalf("delete after endExecute: %v", err)
	}
}

// TestExecuteMultiData は複数データの一括実行を確認する:
// 全件が指定順に返って同時に IN_USE になり、1件でも取得できない場合は
// どのデータの状態も変わらない（all-or-nothing）
func TestExecuteMultiData(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	a := regTest(t, lm, []byte("data-a"))
	b := regTest(t, lm, []byte("data-b"))

	// 指定順どおりに復号データが返り、全件が IN_USE になる
	inputs, err := lm.beginExecute([]string{b.DataID, a.DataID}, testProgram)
	if err != nil {
		t.Fatalf("beginExecute: %v", err)
	}
	if len(inputs) != 2 || !bytes.Equal(inputs[0], []byte("data-b")) || !bytes.Equal(inputs[1], []byte("data-a")) {
		t.Fatalf("inputs = %q, want [data-b data-a]", inputs)
	}
	for _, id := range []string{a.DataID, b.DataID} {
		if info, _ := lm.status(id); info.State != stateInUse {
			t.Fatalf("state of %s = %s, want IN_USE", id, info.State)
		}
	}
	// 使用中のデータが1件でも重なる実行は拒否される（§5 不変条件2）
	if _, err := lm.beginExecute([]string{a.DataID}, testProgram); !errors.Is(err, errBusy) {
		t.Fatalf("overlapping execute: err = %v, want errBusy", err)
	}
	lm.endExecute([]string{b.DataID, a.DataID})
	for _, id := range []string{a.DataID, b.DataID} {
		if info, _ := lm.status(id); info.State != stateRegistered {
			t.Fatalf("state of %s after endExecute = %s, want REGISTERED", id, info.State)
		}
	}

	// all-or-nothing: 存在しないIDが混ざると、他のデータも IN_USE にならない
	if _, err := lm.beginExecute([]string{a.DataID, "d-ffffffffffffffff"}, testProgram); !errors.Is(err, errNotFound) {
		t.Fatalf("execute with unknown id: err = %v, want errNotFound", err)
	}
	if info, _ := lm.status(a.DataID); info.State != stateRegistered {
		t.Fatalf("state after failed multi execute = %s, want REGISTERED", info.State)
	}

	// ホワイトリスト外のデータが混ざる場合も同様（all-or-nothing、§3.4）
	other, err := lm.register([]byte("data-other"), testUploader, []string{testProgram2})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err = lm.beginExecute([]string{a.DataID, other.DataID}, testProgram)
	if !errors.Is(err, errProgramNotAllowed) || !strings.Contains(err.Error(), other.DataID) {
		t.Fatalf("execute with not-allowed id: err = %v, want errProgramNotAllowed mentioning %s", err, other.DataID)
	}
	if info, _ := lm.status(a.DataID); info.State != stateRegistered {
		t.Fatalf("state after failed multi execute = %s, want REGISTERED", info.State)
	}

	// 削除済みIDが混ざる場合も同様。エラーには対象のデータIDが含まれる
	if _, err := lm.delete(b.DataID, testUploader, false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = lm.beginExecute([]string{a.DataID, b.DataID}, testProgram)
	if !errors.Is(err, errDeleted) || !strings.Contains(err.Error(), b.DataID) {
		t.Fatalf("execute with deleted id: err = %v, want errDeleted mentioning %s", err, b.DataID)
	}
	if info, _ := lm.status(a.DataID); info.State != stateRegistered {
		t.Fatalf("state after failed multi execute = %s, want REGISTERED", info.State)
	}

	// ゼロ個の指定は状態に触れず成功する（ステートレス実行に対応）
	inputs, err = lm.beginExecute(nil, testProgram)
	if err != nil || len(inputs) != 0 {
		t.Fatalf("empty execute: inputs=%v err=%v", inputs, err)
	}
}

func TestDeleteIssuesCertificateAndBecomesTerminal(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	rec := regTest(t, lm, []byte("to be deleted"))

	cert, err := lm.delete(rec.DataID, testUploader, false)
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
	// 削除証明にアップローダ識別子は含めない（§7-Q6）
	if bytes.Contains(cert, []byte(testUploader)) {
		t.Fatalf("certificate must not contain the uploader key: %s", cert)
	}

	// DELETED は終端状態（§5 不変条件3）: execute は不可、再削除も不可、proof は取得可
	if _, err := lm.beginExecute([]string{rec.DataID}, testProgram); !errors.Is(err, errDeleted) {
		t.Fatalf("execute after delete: err = %v, want errDeleted", err)
	}
	if _, err := lm.delete(rec.DataID, testUploader, false); !errors.Is(err, errDeleted) {
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
	rec2 := regTest(t, lm, []byte("alive"))
	if _, err := lm.proof(rec2.DataID); !errors.Is(err, errNotDeleted) {
		t.Fatalf("proof on alive data: err = %v, want errNotDeleted", err)
	}
}

// TestDeleteAuthorization は削除の認可（§7-Q3: オーナーまたは記録済みアップローダ）を
// 確認する
func TestDeleteAuthorization(t *testing.T) {
	lm := newTestManager(t, t.TempDir())

	// 無関係な鍵（オーナーでもアップローダでもない）による削除は拒否される
	rec := regTest(t, lm, []byte("private"))
	if _, err := lm.delete(rec.DataID, "c29tZW9uZS1lbHNl", false); !errors.Is(err, errForbidden) {
		t.Fatalf("delete by unrelated key: err = %v, want errForbidden", err)
	}
	// 記録済みアップローダ鍵による削除は成功する
	if _, err := lm.delete(rec.DataID, testUploader, false); err != nil {
		t.Fatalf("delete by uploader: %v", err)
	}

	// オーナー（isOwner=true。ハンドラがオーナー鍵一致を判定済み）による削除は
	// アップローダと無関係に成功する
	rec2 := regTest(t, lm, []byte("owner deletable"))
	if _, err := lm.delete(rec2.DataID, "b3duZXIta2V5", true); err != nil {
		t.Fatalf("delete by owner: %v", err)
	}
}

// TestWhitelistEnforcement はホワイトリスト照合（U5、§3.4）を確認する
func TestWhitelistEnforcement(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	rec := regTest(t, lm, []byte("guarded")) // 許可は testProgram のみ

	// 許可外の program_id での実行は拒否され、エラーに対象のデータIDが含まれる
	_, err := lm.beginExecute([]string{rec.DataID}, testProgram2)
	if !errors.Is(err, errProgramNotAllowed) || !strings.Contains(err.Error(), rec.DataID) {
		t.Fatalf("execute with not-allowed program: err = %v, want errProgramNotAllowed mentioning %s", err, rec.DataID)
	}
	if info, _ := lm.status(rec.DataID); info.State != stateRegistered {
		t.Fatalf("state after rejected execute = %s, want REGISTERED", info.State)
	}

	// 許可されている program_id では実行できる
	if _, err := lm.beginExecute([]string{rec.DataID}, testProgram); err != nil {
		t.Fatalf("execute with allowed program: %v", err)
	}
	lm.endExecute([]string{rec.DataID})
}

// TestSetAllowedPrograms はホワイトリスト編集（T7b、全置換・アップローダ専権）を確認する
func TestSetAllowedPrograms(t *testing.T) {
	dir := t.TempDir()
	lm := newTestManager(t, dir)
	rec, err := lm.register([]byte("editable"), testUploader, nil) // 初期は空＝すべて拒否
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// アップローダ以外（オーナー鍵を含む）による編集は拒否される
	if _, err := lm.setAllowedPrograms(rec.DataID, "b3duZXIta2V5", []string{testProgram}); !errors.Is(err, errForbidden) {
		t.Fatalf("edit by non-uploader: err = %v, want errForbidden", err)
	}

	// アップローダによる全置換。以後の実行に反映される
	if _, err := lm.setAllowedPrograms(rec.DataID, testUploader, []string{testProgram}); err != nil {
		t.Fatalf("setAllowedPrograms: %v", err)
	}
	if _, err := lm.beginExecute([]string{rec.DataID}, testProgram); err != nil {
		t.Fatalf("execute after whitelist add: %v", err)
	}

	// IN_USE 中の編集は許される（実行中の実行には影響しない）。
	// ディスク上には IN_USE を永続化しない不変条件が保たれる
	if _, err := lm.setAllowedPrograms(rec.DataID, testUploader, []string{testProgram, testProgram2}); err != nil {
		t.Fatalf("setAllowedPrograms while IN_USE: %v", err)
	}
	metaRaw, _ := os.ReadFile(lm.store.metaPath(rec.DataID))
	if !bytes.Contains(metaRaw, []byte(`"REGISTERED"`)) {
		t.Fatalf("meta on disk while IN_USE should stay REGISTERED: %s", metaRaw)
	}
	lm.endExecute([]string{rec.DataID})

	// nil（フィールド省略に対応）は空リスト＝すべて拒否に戻す
	if _, err := lm.setAllowedPrograms(rec.DataID, testUploader, nil); err != nil {
		t.Fatalf("setAllowedPrograms(nil): %v", err)
	}
	if _, err := lm.beginExecute([]string{rec.DataID}, testProgram); !errors.Is(err, errProgramNotAllowed) {
		t.Fatalf("execute after whitelist clear: err = %v, want errProgramNotAllowed", err)
	}

	// 再起動後も編集結果が保持される
	lm2 := newTestManager(t, dir)
	if _, err := lm2.beginExecute([]string{rec.DataID}, testProgram); !errors.Is(err, errProgramNotAllowed) {
		t.Fatalf("whitelist after restart: err = %v, want errProgramNotAllowed", err)
	}

	// 未知のIDは errNotFound、削除済みは errDeleted
	if _, err := lm.setAllowedPrograms("d-ffffffffffffffff", testUploader, nil); !errors.Is(err, errNotFound) {
		t.Fatalf("edit unknown id: err = %v, want errNotFound", err)
	}
	if _, err := lm.delete(rec.DataID, testUploader, false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := lm.setAllowedPrograms(rec.DataID, testUploader, []string{testProgram}); !errors.Is(err, errDeleted) {
		t.Fatalf("edit after delete: err = %v, want errDeleted", err)
	}
}

// TestConcurrentDeleteRace は同一データIDへ並行に削除をかけても、
// ちょうど1回だけ成功し他は既削除エラーになることを確認する（TOCTOU対策 §5）
func TestConcurrentDeleteRace(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	rec := regTest(t, lm, []byte("contended"))

	const n = 16
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = lm.delete(rec.DataID, testUploader, false)
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
	alive := regTest(t, lm1, []byte("survives restart"))
	deleted := regTest(t, lm1, []byte("deleted before restart"))
	cert, err := lm1.delete(deleted.DataID, testUploader, false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	// 再起動を模擬: 同じディレクトリから新しいマネージャを作る
	lm2 := newTestManager(t, dir)

	info, err := lm2.status(alive.DataID)
	if err != nil || info.State != stateRegistered {
		t.Fatalf("alive data after restart: info=%+v err=%v", info, err)
	}
	// ホワイトリストも復元されている
	if _, err := lm2.beginExecute([]string{alive.DataID}, testProgram2); !errors.Is(err, errProgramNotAllowed) {
		t.Fatalf("whitelist check after restart: err = %v, want errProgramNotAllowed", err)
	}
	inputs, err := lm2.beginExecute([]string{alive.DataID}, testProgram)
	if err != nil {
		t.Fatalf("execute after restart: %v", err)
	}
	if len(inputs) != 1 || string(inputs[0]) != "survives restart" {
		t.Fatalf("inputs after restart = %q", inputs)
	}
	lm2.endExecute([]string{alive.DataID})

	// アップローダ鍵も復元されている（無関係な鍵の削除は拒否、アップローダは削除可）
	if _, err := lm2.delete(alive.DataID, "c29tZW9uZS1lbHNl", false); !errors.Is(err, errForbidden) {
		t.Fatalf("uploader check after restart: err = %v, want errForbidden", err)
	}
	if _, err := lm2.delete(alive.DataID, testUploader, false); err != nil {
		t.Fatalf("delete by uploader after restart: %v", err)
	}

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
	rec := regTest(t, lm, []byte("shred me"))

	// 削除前: blob が存在し、meta に dek が含まれる
	if _, err := os.Stat(lm.store.blobPath(rec.DataID)); err != nil {
		t.Fatalf("blob missing before delete: %v", err)
	}
	metaRaw, _ := os.ReadFile(lm.store.metaPath(rec.DataID))
	if !bytes.Contains(metaRaw, []byte(`"dek"`)) {
		t.Fatalf("meta before delete should contain dek: %s", metaRaw)
	}

	if _, err := lm.delete(rec.DataID, testUploader, false); err != nil {
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
	rec := regTest(t, lm, []byte("crash victim"))

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
