package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var errProgramNotFound = errors.New("program not found")

// programRecord は事前アップロードされた WASM プログラム1件分のメタデータ。
// sha256 は ID に含まれるため独立フィールドは持たない。
// データ（metaRecord）と異なり状態機械・削除証明は持たない（put / 参照 / 削除のみ）
type programRecord struct {
	ProgramID  string `json:"program_id"`
	Size       int    `json:"size"`        // WASM バイナリのバイト数
	UploadedAt string `json:"uploaded_at"` // RFC3339 UTC
	Uploader   string `json:"uploader"`    // アップロード者の公開鍵（base64）＝オーナー
}

// programID は WASM バイナリのコンテンツアドレス ID（工事計画書 §3.2）:
// "p-" + sha256(バイナリ) の hex 小文字 64 桁。
// ID がコード内容と1対1で結び付くため、アップローダは手元で計算したハッシュを
// ホワイトリストに載せることができ、ID の発行をサーバに信頼委譲する必要がない
func programID(wasm []byte) string {
	sum := sha256.Sum256(wasm)
	return "p-" + hex.EncodeToString(sum[:])
}

// isProgramID は "p-" + 64桁 hex 小文字の形式検査を行う
func isProgramID(id string) bool {
	if len(id) != 66 || !strings.HasPrefix(id, "p-") {
		return false
	}
	for _, c := range id[2:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// programRegistry はプログラムレジストリの唯一の管理者。
// mu が put/get/delete を直列化する（lifecycleManager と同じ「正しさ優先」の設計）
type programRegistry struct {
	mu      sync.Mutex
	entries map[string]*programRecord
	store   *store
}

func newProgramRegistry(st *store) (*programRegistry, error) {
	pr := &programRegistry{entries: map[string]*programRecord{}, store: st}
	recs, err := st.loadProgramMetas()
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		pr.entries[rec.ProgramID] = rec
	}
	return pr, nil
}

// put はプログラムを保存し、レコードと「新規作成されたか」を返す。
// コンテンツアドレスのため同一バイナリの再アップロードは冪等で、既存レコードを
// 上書きせずそのまま返す（衝突時の再生成ロジックは不要）。
// 書き込みは blob → meta の順（register と同じ方式）: blob 書き込み後にクラッシュ
// しても、meta の無い blob はロード対象にならず登録は成立しない
func (pr *programRegistry) put(wasm []byte, uploader string) (*programRecord, bool, error) {
	id := programID(wasm)

	pr.mu.Lock()
	defer pr.mu.Unlock()
	if rec, ok := pr.entries[id]; ok {
		return rec, false, nil
	}
	rec := &programRecord{
		ProgramID:  id,
		Size:       len(wasm),
		UploadedAt: nowRFC3339(),
		Uploader:   uploader,
	}
	if err := pr.store.writeProgramBlob(id, wasm); err != nil {
		return nil, false, err
	}
	if err := pr.store.writeProgramMeta(rec); err != nil {
		return nil, false, err
	}
	pr.entries[id] = rec
	return rec, true, nil
}

// get はプログラム本体を返す。ロード時に blob の sha256 を ID と突き合わせて
// 整合性を検証する（encrypted mount の保護に加えた二重チェック）
func (pr *programRegistry) get(id string) ([]byte, error) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if _, ok := pr.entries[id]; !ok {
		return nil, fmt.Errorf("%s: %w", id, errProgramNotFound)
	}
	wasm, err := pr.store.readProgramBlob(id)
	if err != nil {
		return nil, fmt.Errorf("read program %s: %w", id, err)
	}
	if programID(wasm) != id {
		return nil, fmt.Errorf("program %s: stored binary does not match its content hash", id)
	}
	return wasm, nil
}

// delete はプログラムを削除する。削除は meta → blob の順: meta の除去がコミット
// ポイントで、blob 除去前にクラッシュしても orphan blob はロード対象にならず無害
// （同一バイナリの再アップロードで上書きされる）。
// コンテンツアドレスのため、削除後に同一バイナリを再アップロードすれば同じ ID に
// 復元される（ホワイトリスト側の参照は書き換え不要）
func (pr *programRegistry) delete(id string) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if _, ok := pr.entries[id]; !ok {
		return fmt.Errorf("%s: %w", id, errProgramNotFound)
	}
	if err := pr.store.removeProgramMeta(id); err != nil {
		return err
	}
	delete(pr.entries, id)
	if err := pr.store.removeProgramBlob(id); err != nil {
		return err
	}
	return nil
}
