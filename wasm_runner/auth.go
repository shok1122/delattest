package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// errUnauthorized は署名認証（ヘッダ欠落・不正署名・タイムスタンプ逸脱・リプレイ）の
// 失敗。HTTP では 401 に対応する
var errUnauthorized = errors.New("unauthorized")

// 署名認証（工事計画書 §3.1）で用いるリクエストヘッダ
const (
	headerPublicKey = "X-Public-Key" // Ed25519 公開鍵（raw 32B の base64）
	headerSignature = "X-Signature"  // signedMessage への Ed25519 署名（base64）
	headerTimestamp = "X-Timestamp"  // RFC3339 UTC。リプレイ対策のウィンドウ判定に使う
)

// signedMessage は署名対象メッセージを決定的に再構築できる形へ正規化する:
//
//	<METHOD>\n<PATH>\n<X-Timestamp>\nsha256(<request body>) の hex 小文字
//
// パラメータはすべて JSON ボディに載せる方針（クエリパラメータ不使用）のため、
// PATH はパスのみでよく、ボディハッシュを通じて全パラメータが署名で保護される。
// ボディ全体でなくハッシュに署名することで、大きなボディでも署名検証コストは一定
func signedMessage(method, path, timestamp string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(method + "\n" + path + "\n" + timestamp + "\n" + hex.EncodeToString(sum[:]))
}

// authenticator は署名認証の唯一の実装点。検証は「署名が提示された公開鍵で正当か」
// までを担い、その鍵に操作が許されるか（オーナー鍵か・記録済みアップローダ鍵か）の
// 認可はハンドラ側で行う
type authenticator struct {
	window time.Duration // タイムスタンプの許容ウィンドウ（過去・未来とも）
	mu     sync.Mutex
	seen   map[string]time.Time // 使用済み署名 → 破棄してよい時刻（リプレイ対策）
}

func newAuthenticator(window time.Duration) *authenticator {
	return &authenticator{window: window, seen: map[string]time.Time{}}
}

// authenticate はリクエストの署名を検証し、署名者の公開鍵（canonical な base64）を返す。
// 検証順: ヘッダ形式 → タイムスタンプのウィンドウ → Ed25519 署名 → リプレイ。
// TEE 内の時計はホスト由来で信頼できない点は既知の制約として受容する（§7-Q2）
func (a *authenticator) authenticate(r *http.Request, body []byte) (string, error) {
	pubB64 := r.Header.Get(headerPublicKey)
	sigB64 := r.Header.Get(headerSignature)
	ts := r.Header.Get(headerTimestamp)
	if pubB64 == "" || sigB64 == "" || ts == "" {
		return "", fmt.Errorf("%w: %s, %s and %s headers are required",
			errUnauthorized, headerPublicKey, headerSignature, headerTimestamp)
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: invalid public key (want base64 of raw %d bytes)",
			errUnauthorized, ed25519.PublicKeySize)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: invalid signature encoding", errUnauthorized)
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "", fmt.Errorf("%w: invalid timestamp (want RFC3339)", errUnauthorized)
	}
	now := time.Now()
	if d := now.Sub(t); d > a.window || d < -a.window {
		return "", fmt.Errorf("%w: timestamp outside allowed window (±%s)", errUnauthorized, a.window)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), signedMessage(r.Method, r.URL.Path, ts, body), sig) {
		return "", fmt.Errorf("%w: signature verification failed", errUnauthorized)
	}
	// リプレイ検査は署名検証の後に行う（未検証の値でキャッシュを埋めさせない）。
	// キャッシュのキーはデコード済みバイト列の再エンコードで正規化し、
	// 同じ署名の別 base64 表現による素通りを防ぐ
	key := base64.StdEncoding.EncodeToString(pub) + "|" + base64.StdEncoding.EncodeToString(sig)
	if err := a.checkReplay(key, now); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// checkReplay は使用済み署名キャッシュへの登録と重複検査を行う。
// エントリは「同じタイムスタンプがウィンドウ判定で拒否されるようになる時刻」まで
// 保持すれば十分（timestamp は最大で now+window まで未来を許すため 2*window 保持する）
func (a *authenticator) checkReplay(key string, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, expiry := range a.seen {
		if now.After(expiry) {
			delete(a.seen, k)
		}
	}
	if _, dup := a.seen[key]; dup {
		return fmt.Errorf("%w: replayed signature", errUnauthorized)
	}
	a.seen[key] = now.Add(2 * a.window)
	return nil
}
