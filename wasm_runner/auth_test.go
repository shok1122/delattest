package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKey は署名認証テスト用の Ed25519 鍵ペア
type testKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func genTestKey(t *testing.T) *testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return &testKey{pub: pub, priv: priv}
}

func (k *testKey) pubB64() string {
	return base64.StdEncoding.EncodeToString(k.pub)
}

// headers は署名認証の3ヘッダを組み立てる。タイムスタンプはナノ秒精度
// （RFC3339Nano）を使う: 秒精度だと同一秒内の同一内容のコマンドが同一署名になり、
// リプレイ対策のキャッシュに正当なリクエストが衝突するため（クライアント側の規約）
func (k *testKey) headers(method, path string, body []byte) map[string]string {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	sig := ed25519.Sign(k.priv, signedMessage(method, path, ts, body))
	return map[string]string{
		headerPublicKey: k.pubB64(),
		headerSignature: base64.StdEncoding.EncodeToString(sig),
		headerTimestamp: ts,
	}
}

func TestSignedMessageFormat(t *testing.T) {
	body := []byte(`{"data":"x"}`)
	sum := sha256.Sum256(body)
	want := "POST\n/data\n2026-07-10T00:00:00Z\n" + hex.EncodeToString(sum[:])
	if got := string(signedMessage("POST", "/data", "2026-07-10T00:00:00Z", body)); got != want {
		t.Fatalf("signedMessage = %q, want %q", got, want)
	}
	// 空ボディ（DELETE 等）は空バイト列のハッシュ
	emptySum := sha256.Sum256(nil)
	if got := string(signedMessage("DELETE", "/data/d-1", "ts", nil)); !strings.HasSuffix(got, hex.EncodeToString(emptySum[:])) {
		t.Fatalf("signedMessage with empty body = %q", got)
	}
}

// authReq は署名ヘッダ付きのリクエストを組み立てて authenticate に掛ける
func authReq(a *authenticator, method, path string, body []byte, headers map[string]string) (string, error) {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return a.authenticate(r, body)
}

func TestAuthenticateAcceptsValidSignature(t *testing.T) {
	a := newAuthenticator(5 * time.Minute)
	k := genTestKey(t)
	body := []byte(`{"program_id":"p-x"}`)

	signer, err := authReq(a, "POST", "/execute", body, k.headers("POST", "/execute", body))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if signer != k.pubB64() {
		t.Fatalf("signer = %s, want %s", signer, k.pubB64())
	}
}

func TestAuthenticateRejectsInvalidRequests(t *testing.T) {
	a := newAuthenticator(5 * time.Minute)
	k := genTestKey(t)
	other := genTestKey(t)
	body := []byte(`{"x":1}`)

	valid := func() map[string]string { return k.headers("POST", "/data", body) }

	cases := []struct {
		name    string
		mutate  func(h map[string]string)
		reqBody []byte
		path    string
		method  string
	}{
		{name: "missing public key", mutate: func(h map[string]string) { delete(h, headerPublicKey) }},
		{name: "missing signature", mutate: func(h map[string]string) { delete(h, headerSignature) }},
		{name: "missing timestamp", mutate: func(h map[string]string) { delete(h, headerTimestamp) }},
		{name: "invalid public key encoding", mutate: func(h map[string]string) { h[headerPublicKey] = "???" }},
		{name: "wrong public key length", mutate: func(h map[string]string) {
			h[headerPublicKey] = base64.StdEncoding.EncodeToString([]byte("short"))
		}},
		{name: "invalid signature encoding", mutate: func(h map[string]string) { h[headerSignature] = "???" }},
		{name: "signature by another key", mutate: func(h map[string]string) {
			h[headerPublicKey] = other.pubB64() // 署名は k のまま → 検証失敗
		}},
		{name: "invalid timestamp format", mutate: func(h map[string]string) { h[headerTimestamp] = "yesterday" }},
		{name: "tampered body", reqBody: []byte(`{"x":2}`)},
		{name: "different path", path: "/execute"},
		{name: "different method", method: "PUT"},
	}
	for _, tc := range cases {
		h := valid()
		if tc.mutate != nil {
			tc.mutate(h)
		}
		reqBody := body
		if tc.reqBody != nil {
			reqBody = tc.reqBody
		}
		path := "/data"
		if tc.path != "" {
			path = tc.path
		}
		method := "POST"
		if tc.method != "" {
			method = tc.method
		}
		if _, err := authReq(a, method, path, reqBody, h); !errors.Is(err, errUnauthorized) {
			t.Fatalf("%s: err = %v, want errUnauthorized", tc.name, err)
		}
	}
}

func TestAuthenticateTimestampWindow(t *testing.T) {
	a := newAuthenticator(1 * time.Minute)
	k := genTestKey(t)
	body := []byte(`{}`)

	sign := func(ts string) map[string]string {
		sig := ed25519.Sign(k.priv, signedMessage("POST", "/data", ts, body))
		return map[string]string{
			headerPublicKey: k.pubB64(),
			headerSignature: base64.StdEncoding.EncodeToString(sig),
			headerTimestamp: ts,
		}
	}

	// ウィンドウ内（過去・未来とも）は受理される
	for _, d := range []time.Duration{-30 * time.Second, 30 * time.Second} {
		ts := time.Now().Add(d).UTC().Format(time.RFC3339Nano)
		if _, err := authReq(a, "POST", "/data", body, sign(ts)); err != nil {
			t.Fatalf("timestamp %s: %v", ts, err)
		}
	}
	// ウィンドウ外は拒否される（署名自体は正当でも）
	for _, d := range []time.Duration{-2 * time.Minute, 2 * time.Minute} {
		ts := time.Now().Add(d).UTC().Format(time.RFC3339Nano)
		if _, err := authReq(a, "POST", "/data", body, sign(ts)); !errors.Is(err, errUnauthorized) {
			t.Fatalf("timestamp %s: err = %v, want errUnauthorized", ts, err)
		}
	}
}

func TestAuthenticateRejectsReplay(t *testing.T) {
	a := newAuthenticator(5 * time.Minute)
	k := genTestKey(t)
	body := []byte(`{"data":"x"}`)
	h := k.headers("POST", "/data", body)

	if _, err := authReq(a, "POST", "/data", body, h); err != nil {
		t.Fatalf("first request: %v", err)
	}
	// まったく同じヘッダ（＝同じ署名）の再送はリプレイとして拒否される
	if _, err := authReq(a, "POST", "/data", body, h); !errors.Is(err, errUnauthorized) {
		t.Fatalf("replayed request: err = %v, want errUnauthorized", err)
	}
	// 新しいタイムスタンプで署名し直せば通る
	if _, err := authReq(a, "POST", "/data", body, k.headers("POST", "/data", body)); err != nil {
		t.Fatalf("re-signed request: %v", err)
	}
}
