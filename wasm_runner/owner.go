package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
)

// オーナー鍵登録（TOFU）のエラー種別。HTTP への対応付けはハンドラ側で行う
var (
	errOwnerExists = errors.New("owner key already registered") // 409
	errNoOwner     = errors.New("owner key not registered")     // 403（未登録時はコマンドを拒否）
)

// ownerRecord はランナーオーナーの登録情報。封印ストレージ（owner.json）に永続化される
type ownerRecord struct {
	PublicKey    string `json:"public_key"`    // Ed25519 公開鍵（raw 32B の base64）
	RegisteredAt string `json:"registered_at"` // RFC3339 UTC
}

// ownerManager はオーナー鍵の唯一の管理者（工事計画書 §3.1 案B: TOFU）。
// 初回の register だけを受け付け、以後は変更不可。登録済み状態は封印ストレージに
// 永続化され、再起動後も維持される
type ownerManager struct {
	mu    sync.Mutex
	rec   *ownerRecord
	store *store
}

func newOwnerManager(st *store) (*ownerManager, error) {
	rec, err := st.loadOwner()
	if err != nil {
		return nil, err
	}
	return &ownerManager{rec: rec, store: st}, nil
}

// register はオーナー公開鍵を初回のみ登録する（TOFU）。登録済みなら errOwnerExists。
// 保存する鍵はデコード済みバイト列の再エンコードで正規化し、以後の一致比較を
// 単純な文字列比較で行えるようにする
func (om *ownerManager) register(pubB64 string) (*ownerRecord, error) {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key (want base64 of raw %d bytes)", ed25519.PublicKeySize)
	}

	om.mu.Lock()
	defer om.mu.Unlock()
	if om.rec != nil {
		return nil, errOwnerExists
	}
	rec := &ownerRecord{
		PublicKey:    base64.StdEncoding.EncodeToString(pub),
		RegisteredAt: nowRFC3339(),
	}
	if err := om.store.writeOwner(rec); err != nil {
		return nil, err
	}
	om.rec = rec
	return rec, nil
}

// key は登録済みオーナー公開鍵（base64）を返す。未登録なら ok = false
func (om *ownerManager) key() (string, bool) {
	om.mu.Lock()
	defer om.mu.Unlock()
	if om.rec == nil {
		return "", false
	}
	return om.rec.PublicKey, true
}
