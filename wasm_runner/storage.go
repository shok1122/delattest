package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// store は封印ストレージ（§6.2）のファイル配置を担う。
//
// 2層の暗号化で保護される:
//  1. Gramine 実行時、baseDir は encrypted mount（Protected Files）を指すため、
//     ホスト上のファイルはすべて Gramine により透過的に暗号化される（鍵はエンクレーブ外に出ない）
//  2. データ本体はさらにデータごとの鍵（DEK）で AES-256-GCM 暗号化して保存する。
//     削除時に DEK を破棄することで、暗号化 blob やそのバックアップが残存しても
//     復号不能にする（クリプトシュレッディング §9.1）
//
// 配置: baseDir/meta/<id>.json（メタデータ）, baseDir/blobs/<id>.bin（暗号化データ本体）
type store struct {
	baseDir string
}

func newStore(dir string) (*store, error) {
	for _, d := range []string{filepath.Join(dir, "meta"), filepath.Join(dir, "blobs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &store{baseDir: dir}, nil
}

func (s *store) metaPath(id string) string { return filepath.Join(s.baseDir, "meta", id+".json") }
func (s *store) blobPath(id string) string { return filepath.Join(s.baseDir, "blobs", id+".bin") }
func (s *store) usersPath() string         { return filepath.Join(s.baseDir, "users.json") }

// atomicWrite は一時ファイルに書き切ってから rename する。
// クラッシュしても書きかけのファイルが正規の名前で残らないようにするため
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *store) writeMeta(rec *metaRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicWrite(s.metaPath(rec.DataID), b)
}

func (s *store) loadMetas() ([]*metaRecord, error) {
	entries, err := os.ReadDir(filepath.Join(s.baseDir, "meta"))
	if err != nil {
		return nil, err
	}
	var recs []*metaRecord
	for _, e := range entries {
		// 書きかけの .tmp などはロード対象にしない
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.baseDir, "meta", e.Name()))
		if err != nil {
			return nil, err
		}
		var rec metaRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil, err
		}
		recs = append(recs, &rec)
	}
	return recs, nil
}

// writeUsers / loadUsers はユーザ表（§4.1 認証）を単一の JSON ファイルとして
// 永続化する。DATA_DIR 以下に置くため、Gramine 実行時は meta/blobs と同様に
// encrypted mount の保護下に入る
func (s *store) writeUsers(recs []*userRecord) error {
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	return atomicWrite(s.usersPath(), b)
}

func (s *store) loadUsers() ([]*userRecord, error) {
	b, err := os.ReadFile(s.usersPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []*userRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

func (s *store) writeBlob(id string, dek, plaintext []byte) error {
	sealed, err := sealBytes(dek, plaintext)
	if err != nil {
		return err
	}
	return atomicWrite(s.blobPath(id), sealed)
}

func (s *store) readBlob(id string, dek []byte) ([]byte, error) {
	sealed, err := os.ReadFile(s.blobPath(id))
	if err != nil {
		return nil, err
	}
	return openBytes(dek, sealed)
}

func (s *store) removeBlob(id string) error {
	err := os.Remove(s.blobPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// sealBytes はデータごとの鍵（DEK, 32バイト）で AES-256-GCM 暗号化する。
// 出力フォーマットは nonce || ciphertext（認証タグ込み）
func sealBytes(dek, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func openBytes(dek, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed blob too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}
