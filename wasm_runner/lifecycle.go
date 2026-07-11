package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"
)

// dataState はデータのライフサイクル状態（§5）
type dataState string

const (
	stateRegistered dataState = "REGISTERED"
	stateInUse      dataState = "IN_USE"
	stateDeleting   dataState = "DELETING"
	stateDeleted    dataState = "DELETED" // 終端状態。データIDの再利用不可
)

// ライフサイクル操作のエラー種別。HTTPステータスへの対応付けは handlers 側で行う
var (
	errNotFound          = errors.New("data not found")
	errBusy              = errors.New("data is busy (IN_USE or DELETING)")
	errDeleted           = errors.New("data already deleted")
	errNotDeleted        = errors.New("data is not deleted yet")
	errForbidden         = errors.New("requester key is not authorized for this data")
	errProgramNotAllowed = errors.New("program is not in the allowed_programs of this data")
)

// metaRecord は §6.1 のメタデータストア1件分。封印ストレージに永続化される。
// DEK はデータ本体の暗号鍵で、削除時にレコードから取り除かれる（クリプトシュレッディング §9.1）。
// DELETED のレコードは削除証明の再取得（§7 proof）と ID 再利用の禁止（§5 不変条件3）の
// ために永続的に残す
type metaRecord struct {
	DataID      string    `json:"data_id"`
	State       dataState `json:"state"`
	ContentHash string    `json:"content_hash"` // "sha256:<hex>"（登録時点の元データのハッシュ）
	CreatedAt   string    `json:"created_at"`   // RFC3339 UTC
	// Uploader はアップローダの公開鍵（Ed25519 raw 32B の base64）。
	// 「誰がアップロードしたか」の識別（U3）に加え、ホワイトリスト編集・削除の
	// 認可主体の特定（U5、§3.4）に使う。削除証明には含めない（§7-Q6）
	Uploader string `json:"uploader"`
	// AllowedPrograms はこのデータに対して実行を許可する program_id の集合（U5）。
	// 空＝すべて拒否（deny by default、§7-Q8）。状態機械の状態ではなく可変メタデータ
	AllowedPrograms []string        `json:"allowed_programs"`
	DEK             []byte          `json:"dek,omitempty"`
	DeletedAt       string          `json:"deleted_at,omitempty"`
	Certificate     json.RawMessage `json:"certificate,omitempty"`
}

// lifecycleManager は状態機械の唯一の管理者であり、状態を書き換えられる経路を
// ここに一本化する（§5 不変条件4）。mu が全レコードの状態遷移を直列化することで、
// IN_USE/DELETING 中の競合操作（TOCTOU）を防ぐ（§5 不変条件2）。
// WASM 実行そのものは mu の外で行われ、その間は state = IN_USE が排他を担う
type lifecycleManager struct {
	mu      sync.Mutex
	entries map[string]*metaRecord
	store   *store
	prover  *prover
}

func newLifecycleManager(st *store, pr *prover) (*lifecycleManager, error) {
	lm := &lifecycleManager{entries: map[string]*metaRecord{}, store: st, prover: pr}
	recs, err := st.loadMetas()
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		lm.entries[rec.DataID] = rec
		switch rec.State {
		case stateInUse:
			// 実行途中でプロセスが落ちた場合（通常 IN_USE は永続化されないため防御的措置）。
			// データ自体は残っているので待機状態に戻す
			rec.State = stateRegistered
			if err := st.writeMeta(rec); err != nil {
				return nil, err
			}
			log.Printf("recovered %s: IN_USE -> REGISTERED", rec.DataID)
		case stateDeleting:
			// 削除途中でプロセスが落ちた場合。DEK は既に破棄済みなので削除を完遂する
			if err := lm.finishDelete(rec); err != nil {
				return nil, fmt.Errorf("resume deletion of %s: %w", rec.DataID, err)
			}
			log.Printf("recovered %s: resumed and completed deletion", rec.DataID)
		}
	}
	return lm, nil
}

// register はデータを封印保存し、REGISTERED 状態のレコードを作成する。
// uploader（署名検証済みのアップローダ公開鍵）は必須。allowedPrograms は
// 省略（nil）可能で、その場合は空リスト＝すべて拒否（deny by default）になる
func (lm *lifecycleManager) register(data []byte, uploader string, allowedPrograms []string) (*metaRecord, error) {
	if uploader == "" {
		return nil, errors.New("uploader key required")
	}
	if allowedPrograms == nil {
		allowedPrograms = []string{}
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	id, err := lm.newDataID()
	if err != nil {
		return nil, err
	}

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	rec := &metaRecord{
		DataID:          id,
		State:           stateRegistered,
		ContentHash:     "sha256:" + hex.EncodeToString(sum[:]),
		CreatedAt:       nowRFC3339(),
		Uploader:        uploader,
		AllowedPrograms: allowedPrograms,
		DEK:             dek,
	}

	// blob → meta の順に書く。blob 書き込み後にクラッシュしても、meta の無い blob は
	// 起動時のロード対象にならず登録は成立しない（orphan blob は復号鍵も無く無害）
	if err := lm.store.writeBlob(id, dek, data); err != nil {
		return nil, err
	}
	if err := lm.store.writeMeta(rec); err != nil {
		return nil, err
	}
	lm.entries[id] = rec
	return rec, nil
}

// newDataID は "d-" + 8バイト乱数の ID を生成する。DELETED のレコードも entries に
// 残り続けるため、削除済み ID との重複（再登録）も起こらない（§5 不変条件3）
func (lm *lifecycleManager) newDataID() (string, error) {
	for range 10 {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := "d-" + hex.EncodeToString(b)
		if _, exists := lm.entries[id]; !exists {
			return id, nil
		}
	}
	return "", errors.New("could not allocate unique data id")
}

// beginExecute は指定された全データを REGISTERED -> IN_USE に遷移させ、
// 復号済みのデータ本体を指定順に返す（ids が空なら何もせず nil を返す）。
// 「誰が実行できるか」（オーナー署名）はハンドラ側で検証済みで、ここでは
// 「どのプログラムで実行できるか」＝データごとのホワイトリスト照合（U5、§3.4）を行う。
// 照合は execute 開始時点のスナップショットで、実行途中のホワイトリスト編集は
// 当該実行に影響しない。
// 取得は all-or-nothing: 1件でも検証・復号に失敗した場合、どのデータの状態も
// 変更せずにエラーを返す（エラーには対象のデータIDが付与される）。
// 成功した場合、呼び出し側は実行完了後に必ず同じ ids で endExecute を呼ぶこと。
// IN_USE はメモリ上だけの一時状態として扱い永続化しない（クラッシュ時はディスク上の
// REGISTERED のまま次回起動で復帰する）
func (lm *lifecycleManager) beginExecute(ids []string, programID string) ([][]byte, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// 全件を検証してから状態を変更する。mu が全遷移を直列化しているため、
	// 検証と遷移の間に他のリクエストが割り込むことはない
	recs := make([]*metaRecord, len(ids))
	for i, id := range ids {
		rec, ok := lm.entries[id]
		if !ok {
			return nil, fmt.Errorf("%s: %w", id, errNotFound)
		}
		if !slices.Contains(rec.AllowedPrograms, programID) {
			return nil, fmt.Errorf("%s: %w", id, errProgramNotAllowed)
		}
		switch rec.State {
		case stateDeleted:
			return nil, fmt.Errorf("%s: %w", id, errDeleted)
		case stateInUse, stateDeleting:
			return nil, fmt.Errorf("%s: %w", id, errBusy)
		}
		recs[i] = rec
	}

	inputs := make([][]byte, len(ids))
	for i, id := range ids {
		input, err := lm.store.readBlob(id, recs[i].DEK)
		if err != nil {
			return nil, fmt.Errorf("read sealed data %s: %w", id, err)
		}
		inputs[i] = input
	}
	for _, rec := range recs {
		rec.State = stateInUse
	}
	return inputs, nil
}

// endExecute は指定された全データを IN_USE -> REGISTERED に戻す（execute complete, §5）
func (lm *lifecycleManager) endExecute(ids []string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	for _, id := range ids {
		if rec, ok := lm.entries[id]; ok && rec.State == stateInUse {
			rec.State = stateRegistered
		}
	}
}

// setAllowedPrograms はホワイトリストを全置換する（差分 API は設けない。冪等で単純、§3.4）。
// 編集できるのは当該データに記録済みのアップローダ鍵の署名者のみ（オーナー鍵では不可。
// オーナーが自分に都合よく許可を広げることを防ぐのが U5 の目的のため）。
// 編集は削除系状態（DELETING/DELETED）に入るまで可能で、IN_USE 中も認める
// （実行中の編集はこのロックで直列化され、実行への適用は次回 execute から）
func (lm *lifecycleManager) setAllowedPrograms(id, requester string, programs []string) (*metaRecord, error) {
	if programs == nil {
		programs = []string{}
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	rec, ok := lm.entries[id]
	if !ok {
		return nil, errNotFound
	}
	if requester != rec.Uploader {
		return nil, errForbidden
	}
	switch rec.State {
	case stateDeleted:
		return nil, errDeleted
	case stateDeleting:
		return nil, errBusy
	}

	old := rec.AllowedPrograms
	rec.AllowedPrograms = programs
	// IN_USE はメモリ上だけの一時状態で永続化しない、という不変条件を保つため、
	// 実行中の編集はディスク上では REGISTERED として書き出す
	persist := *rec
	if persist.State == stateInUse {
		persist.State = stateRegistered
	}
	if err := lm.store.writeMeta(&persist); err != nil {
		rec.AllowedPrograms = old
		return nil, fmt.Errorf("persist allowed_programs: %w", err)
	}
	return rec, nil
}

// delete はデータを削除し、削除証明を発行して返す（§9）。
// 削除できるのはランナーオーナー（isOwner はハンドラがオーナー鍵との一致で判定済み）
// または当該データに記録済みのアップローダ鍵の署名者（§7-Q3: 両方可）
func (lm *lifecycleManager) delete(id, requester string, isOwner bool) (json.RawMessage, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	rec, ok := lm.entries[id]
	if !ok {
		return nil, errNotFound
	}
	if !isOwner && requester != rec.Uploader {
		return nil, errForbidden
	}
	switch rec.State {
	case stateDeleted:
		return nil, errDeleted
	case stateInUse, stateDeleting:
		return nil, errBusy
	}

	// DEK を取り除いた DELETING 状態を永続化する。この書き込みがクリプトシュレッディングの
	// コミットポイントであり、成功以降は（途中でクラッシュしても）復号鍵が復元される経路は
	// 存在せず、次回起動時に削除が完遂される
	dek := rec.DEK
	rec.State = stateDeleting
	rec.DEK = nil
	if err := lm.store.writeMeta(rec); err != nil {
		rec.State = stateRegistered
		rec.DEK = dek
		return nil, fmt.Errorf("persist DELETING state: %w", err)
	}
	wipe(dek)

	if err := lm.finishDelete(rec); err != nil {
		return nil, err
	}
	return rec.Certificate, nil
}

// finishDelete は DELETING 状態のレコードに対し、封印データの消去・削除証明の発行・
// DELETED への遷移を行う
func (lm *lifecycleManager) finishDelete(rec *metaRecord) error {
	if err := lm.store.removeBlob(rec.DataID); err != nil {
		return fmt.Errorf("erase sealed data: %w", err)
	}
	rec.DeletedAt = nowRFC3339()
	cert, err := lm.prover.issueCertificate(rec.DataID, rec.ContentHash, rec.DeletedAt)
	if err != nil {
		return fmt.Errorf("issue deletion certificate: %w", err)
	}
	rec.Certificate = cert
	rec.State = stateDeleted
	return lm.store.writeMeta(rec)
}

// statusInfo は status API の応答。生データや鍵は一切含めない（§5 不変条件1）
type statusInfo struct {
	DataID       string    `json:"data_id"`
	State        dataState `json:"state"`
	RegisteredAt string    `json:"registered_at"`
	DeletedAt    string    `json:"deleted_at,omitempty"`
}

func (lm *lifecycleManager) status(id string) (*statusInfo, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	rec, ok := lm.entries[id]
	if !ok {
		return nil, errNotFound
	}
	return &statusInfo{
		DataID:       rec.DataID,
		State:        rec.State,
		RegisteredAt: rec.CreatedAt,
		DeletedAt:    rec.DeletedAt,
	}, nil
}

// proof は DELETED 状態のデータに対し、発行済みの削除証明を再取得する（監査用 §7）
func (lm *lifecycleManager) proof(id string) (json.RawMessage, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	rec, ok := lm.entries[id]
	if !ok {
		return nil, errNotFound
	}
	if rec.State != stateDeleted {
		return nil, errNotDeleted
	}
	return rec.Certificate, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
