package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
)

// errUnauthorized は API キーが未提示・無効な場合のエラー。HTTP では 401 に対応する
var errUnauthorized = errors.New("invalid or missing api key")

// userRecord はユーザ1件分。API キーの平文は保存せず sha256 ハッシュだけを持つ。
// owner_id は秘密ではない恒久的な識別子であり、データレコード・削除証明・ログに
// 載せてよい。秘密（API キー）と識別子（owner_id）を分離することで、キー漏洩時は
// ユーザ表のエントリ差し替えだけで失効でき、データレコード側は影響を受けない
type userRecord struct {
	OwnerID    string `json:"owner_id"`
	APIKeyHash string `json:"api_key_hash"` // sha256(APIキー) の hex
	CreatedAt  string `json:"created_at"`   // RFC3339 UTC
}

// userManager はユーザ表（API キーハッシュ ↔ owner_id）の唯一の管理者。
// 認証（API キー → owner_id の解決）を担い、認可（データレコードの owner_id との
// 照合）は lifecycleManager 側で行う。ユーザ表は封印ストレージに永続化される
type userManager struct {
	mu     sync.Mutex
	users  []*userRecord
	byHash map[string]*userRecord
	store  *store
}

func newUserManager(st *store) (*userManager, error) {
	um := &userManager{byHash: map[string]*userRecord{}, store: st}
	recs, err := st.loadUsers()
	if err != nil {
		return nil, err
	}
	um.users = recs
	for _, rec := range recs {
		um.byHash[rec.APIKeyHash] = rec
	}
	return um, nil
}

// createUser は owner_id と API キーを新規発行する。キーの平文はこの戻り値限りで、
// 以後サーバはハッシュしか持たない
func (um *userManager) createUser() (*userRecord, string, error) {
	um.mu.Lock()
	defer um.mu.Unlock()

	id, err := um.newOwnerID()
	if err != nil {
		return nil, "", err
	}
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, "", err
	}
	apiKey := "ak-" + hex.EncodeToString(keyBytes)

	rec := &userRecord{
		OwnerID:    id,
		APIKeyHash: hashAPIKey(apiKey),
		CreatedAt:  nowRFC3339(),
	}
	um.users = append(um.users, rec)
	if err := um.store.writeUsers(um.users); err != nil {
		um.users = um.users[:len(um.users)-1]
		return nil, "", err
	}
	um.byHash[rec.APIKeyHash] = rec
	return rec, apiKey, nil
}

// newOwnerID は "u-" + 8バイト乱数の ID を生成する（データIDと同形式）
func (um *userManager) newOwnerID() (string, error) {
	for range 10 {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := "u-" + hex.EncodeToString(b)
		exists := false
		for _, rec := range um.users {
			if rec.OwnerID == id {
				exists = true
				break
			}
		}
		if !exists {
			return id, nil
		}
	}
	return "", errors.New("could not allocate unique owner id")
}

// resolveOwner は API キーを認証し、対応する owner_id を返す。
// ハッシュをキーにした map 引きのため、キー平文同士の比較は発生しない
func (um *userManager) resolveOwner(apiKey string) (string, error) {
	if apiKey == "" {
		return "", errUnauthorized
	}
	um.mu.Lock()
	defer um.mu.Unlock()
	rec, ok := um.byHash[hashAPIKey(apiKey)]
	if !ok {
		return "", errUnauthorized
	}
	return rec.OwnerID, nil
}

func hashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}
