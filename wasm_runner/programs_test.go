package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

const testProgramUploader = "b3duZXIta2V5LWJhc2U2NA=="

func newTestProgramRegistry(t *testing.T, dir string) *programRegistry {
	t.Helper()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	pr, err := newProgramRegistry(st)
	if err != nil {
		t.Fatalf("newProgramRegistry: %v", err)
	}
	return pr
}

func TestProgramIDFormat(t *testing.T) {
	wasm := []byte("dummy wasm bytes")
	sum := sha256.Sum256(wasm)
	want := "p-" + hex.EncodeToString(sum[:])
	if got := programID(wasm); got != want {
		t.Fatalf("programID = %s, want %s", got, want)
	}
	if !isProgramID(want) {
		t.Fatalf("isProgramID(%s) = false, want true", want)
	}
	for _, bad := range []string{
		"", "p-", "d-" + hex.EncodeToString(sum[:]),
		"p-" + hex.EncodeToString(sum[:])[:63],       // 短い
		"p-" + hex.EncodeToString(sum[:])[:63] + "G", // hex でない
		"p-" + hex.EncodeToString(sum[:])[:63] + "A", // 大文字 hex は不可
	} {
		if isProgramID(bad) {
			t.Fatalf("isProgramID(%q) = true, want false", bad)
		}
	}
}

func TestProgramPutGetDelete(t *testing.T) {
	pr := newTestProgramRegistry(t, t.TempDir())
	wasm := noopWasm()

	rec, created, err := pr.put(wasm, testProgramUploader)
	if err != nil || !created {
		t.Fatalf("put: rec=%+v created=%v err=%v", rec, created, err)
	}
	if rec.ProgramID != programID(wasm) || rec.Size != len(wasm) || rec.Uploader != testProgramUploader || rec.UploadedAt == "" {
		t.Fatalf("unexpected record: %+v", rec)
	}

	// 冪等: 同一バイナリの再アップロードは同じ ID を返し、新規作成にならない
	rec2, created, err := pr.put(wasm, testProgramUploader)
	if err != nil || created || rec2.ProgramID != rec.ProgramID {
		t.Fatalf("idempotent put: rec=%+v created=%v err=%v", rec2, created, err)
	}

	got, err := pr.get(rec.ProgramID)
	if err != nil || !bytes.Equal(got, wasm) {
		t.Fatalf("get: %q err=%v", got, err)
	}
	if _, err := pr.get("p-ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"); !errors.Is(err, errProgramNotFound) {
		t.Fatalf("get unknown: err = %v, want errProgramNotFound", err)
	}

	// 削除後は取得できず、ファイルも残らない。再アップロードで同じ ID に復元される
	if err := pr.delete(rec.ProgramID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := pr.get(rec.ProgramID); !errors.Is(err, errProgramNotFound) {
		t.Fatalf("get after delete: err = %v, want errProgramNotFound", err)
	}
	for _, p := range []string{pr.store.programMetaPath(rec.ProgramID), pr.store.programBlobPath(rec.ProgramID)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists after delete", p)
		}
	}
	if err := pr.delete(rec.ProgramID); !errors.Is(err, errProgramNotFound) {
		t.Fatalf("double delete: err = %v, want errProgramNotFound", err)
	}
	rec3, created, err := pr.put(wasm, testProgramUploader)
	if err != nil || !created || rec3.ProgramID != rec.ProgramID {
		t.Fatalf("re-upload after delete: rec=%+v created=%v err=%v", rec3, created, err)
	}
}

// TestProgramPersistenceAcrossRestart は再起動後もレジストリが復元されることを確認する
func TestProgramPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	pr1 := newTestProgramRegistry(t, dir)
	rec, _, err := pr1.put(noopWasm(), testProgramUploader)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	pr2 := newTestProgramRegistry(t, dir)
	got, err := pr2.get(rec.ProgramID)
	if err != nil || !bytes.Equal(got, noopWasm()) {
		t.Fatalf("get after restart: %q err=%v", got, err)
	}
}

// TestProgramIntegrityCheck はロード時のコンテンツハッシュ照合（§3.2 の二重チェック）を
// 確認する: blob が ID のハッシュと一致しなければ実行に使えない
func TestProgramIntegrityCheck(t *testing.T) {
	pr := newTestProgramRegistry(t, t.TempDir())
	rec, _, err := pr.put(noopWasm(), testProgramUploader)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// blob を直接改ざんする（encrypted mount を迂回した書き換えを模擬）
	if err := os.WriteFile(pr.store.programBlobPath(rec.ProgramID), []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := pr.get(rec.ProgramID); err == nil {
		t.Fatalf("get of tampered program should fail")
	}
}
