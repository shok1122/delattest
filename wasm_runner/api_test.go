package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	lm := newTestManager(t, t.TempDir())
	sb := &sandbox{execTimeout: 10 * time.Second, memLimitPages: 1024}
	srv := httptest.NewServer(newHandler(lm, sb))
	t.Cleanup(srv.Close)
	return srv
}

func doReq(t *testing.T, method, url string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestAPILifecycleFlow は register -> status -> execute -> delete -> proof の
// 一連の流れを HTTP 経由で検証する
func TestAPILifecycleFlow(t *testing.T) {
	srv := newTestServer(t)
	data := []byte("lifecycle test data")

	// 登録
	code, body := doReq(t, "POST", srv.URL+"/data", data, nil)
	if code != http.StatusCreated {
		t.Fatalf("register: code=%d body=%s", code, body)
	}
	var reg struct {
		DataID       string `json:"data_id"`
		RegisteredAt string `json:"registered_at"`
	}
	if err := json.Unmarshal(body, &reg); err != nil || reg.DataID == "" || reg.RegisteredAt == "" {
		t.Fatalf("register response invalid: %s (err=%v)", body, err)
	}

	// 状態確認
	code, body = doReq(t, "GET", srv.URL+"/data/"+reg.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
		t.Fatalf("status: code=%d body=%s", code, body)
	}

	// 実行（no-op モジュール）
	code, body = doReq(t, "POST", srv.URL+"/data/"+reg.DataID+"/execute", noopWasm(), nil)
	if code != http.StatusOK {
		t.Fatalf("execute: code=%d body=%s", code, body)
	}

	// 削除 → 削除証明が返る
	code, certBody := doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", code, certBody)
	}
	verifyCertificate(t, certBody, reg.DataID, data)

	// 状態は DELETED
	code, body = doReq(t, "GET", srv.URL+"/data/"+reg.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"DELETED"`) {
		t.Fatalf("status after delete: code=%d body=%s", code, body)
	}

	// 証明の再取得（監査用）は削除時のものと一致
	code, proofBody := doReq(t, "GET", srv.URL+"/data/"+reg.DataID+"/proof", nil, nil)
	if code != http.StatusOK || !bytes.Equal(proofBody, certBody) {
		t.Fatalf("proof: code=%d body=%s", code, proofBody)
	}

	// DELETED への execute は 404（§5 不変条件3）
	code, _ = doReq(t, "POST", srv.URL+"/data/"+reg.DataID+"/execute", noopWasm(), nil)
	if code != http.StatusNotFound {
		t.Fatalf("execute after delete: code=%d, want 404", code)
	}

	// 再削除は 409
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("double delete: code=%d, want 409", code)
	}
}

// verifyCertificate は削除証明を検証者の立場で検証する（§9.3 の署名・ハッシュ部分）
func verifyCertificate(t *testing.T, certBody []byte, dataID string, original []byte) {
	t.Helper()
	var cert deletionCertificate
	if err := json.Unmarshal(certBody, &cert); err != nil {
		t.Fatalf("certificate is not valid JSON: %v (%s)", err, certBody)
	}
	if cert.DataID != dataID {
		t.Fatalf("cert.data_id = %s, want %s", cert.DataID, dataID)
	}

	// content_hash が登録したデータの sha256 と一致（§9.3 手順4）
	sum := sha256.Sum256(original)
	if want := "sha256:" + hex.EncodeToString(sum[:]); cert.ContentHash != want {
		t.Fatalf("cert.content_hash = %s, want %s", cert.ContentHash, want)
	}

	// signature が public_key で検証できる（§9.3 手順3）。
	// 署名対象は certificateCore フィールドを順に並べた JSON
	payload, err := json.Marshal(certificateCore{
		DataID:      cert.DataID,
		DeletedAt:   cert.DeletedAt,
		ContentHash: cert.ContentHash,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	pub, err := base64.StdEncoding.DecodeString(cert.EnclaveReport.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("invalid public key: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(cert.Signature)
	if err != nil {
		t.Fatalf("invalid signature encoding: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		t.Fatalf("signature verification failed")
	}
}

func TestAPIRegisterValidation(t *testing.T) {
	srv := newTestServer(t)

	// 空ボディは登録できない
	code, _ := doReq(t, "POST", srv.URL+"/data", nil, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("empty register: code=%d, want 400", code)
	}

	// 未知のIDへの操作は 404
	for _, r := range [][2]string{
		{"POST", "/data/d-ffffffffffffffff/execute"},
		{"DELETE", "/data/d-ffffffffffffffff"},
		{"GET", "/data/d-ffffffffffffffff/status"},
		{"GET", "/data/d-ffffffffffffffff/proof"},
	} {
		body := []byte(nil)
		if r[0] == "POST" {
			body = noopWasm()
		}
		code, _ := doReq(t, r[0], srv.URL+r[1], body, nil)
		if code != http.StatusNotFound {
			t.Fatalf("%s %s: code=%d, want 404", r[0], r[1], code)
		}
	}
}

func TestAPIOwnerToken(t *testing.T) {
	srv := newTestServer(t)
	headers := map[string]string{"X-Owner-Token": "s3cret"}

	code, body := doReq(t, "POST", srv.URL+"/data", []byte("guarded"), headers)
	if code != http.StatusCreated {
		t.Fatalf("register: code=%d body=%s", code, body)
	}
	var reg struct {
		DataID string `json:"data_id"`
	}
	_ = json.Unmarshal(body, &reg)

	// トークン無し / 誤りは 403
	code, _ = doReq(t, "POST", srv.URL+"/data/"+reg.DataID+"/execute", noopWasm(), nil)
	if code != http.StatusForbidden {
		t.Fatalf("execute without token: code=%d, want 403", code)
	}
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, map[string]string{"X-Owner-Token": "wrong"})
	if code != http.StatusForbidden {
		t.Fatalf("delete with wrong token: code=%d, want 403", code)
	}

	// Authorization: Bearer でも渡せる
	code, _ = doReq(t, "POST", srv.URL+"/data/"+reg.DataID+"/execute", noopWasm(),
		map[string]string{"Authorization": "Bearer s3cret"})
	if code != http.StatusOK {
		t.Fatalf("execute with bearer token: code=%d, want 200", code)
	}
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, headers)
	if code != http.StatusOK {
		t.Fatalf("delete with token: code=%d, want 200", code)
	}
}

func TestAPIStatelessExecute(t *testing.T) {
	srv := newTestServer(t)

	code, _ := doReq(t, "POST", srv.URL+"/execute-wasm", noopWasm(), nil)
	if code != http.StatusOK {
		t.Fatalf("stateless execute: code=%d, want 200", code)
	}

	// 壊れたバイナリはエラーテキストを返す（既存挙動の維持）
	code, body := doReq(t, "POST", srv.URL+"/execute-wasm", []byte("not wasm"), nil)
	if code != http.StatusBadRequest || !strings.HasPrefix(string(body), "WASM error:") {
		t.Fatalf("stateless execute invalid: code=%d body=%s", code, body)
	}
}

func TestAPIHealth(t *testing.T) {
	srv := newTestServer(t)
	code, body := doReq(t, "GET", srv.URL+"/", nil, nil)
	if code != http.StatusOK || !strings.HasPrefix(string(body), "OK") {
		t.Fatalf("health: code=%d body=%s", code, body)
	}
}

// TestAPIExecuteBusyConflict は実行中のデータへの競合操作が 409 になることを確認する
func TestAPIExecuteBusyConflict(t *testing.T) {
	lm := newTestManager(t, t.TempDir())
	sb := &sandbox{execTimeout: 10 * time.Second, memLimitPages: 1024}
	srv := httptest.NewServer(newHandler(lm, sb))
	t.Cleanup(srv.Close)

	rec, _ := lm.register([]byte("busy data"), "")

	// IN_USE 状態を直接作る（実行中の状態を模擬）
	if _, err := lm.beginExecute(rec.DataID, ""); err != nil {
		t.Fatalf("beginExecute: %v", err)
	}

	code, _ := doReq(t, "POST", srv.URL+"/data/"+rec.DataID+"/execute", noopWasm(), nil)
	if code != http.StatusConflict {
		t.Fatalf("execute while IN_USE: code=%d, want 409", code)
	}
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+rec.DataID, nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("delete while IN_USE: code=%d, want 409", code)
	}
	code, body := doReq(t, "GET", srv.URL+"/data/"+rec.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"IN_USE"`) {
		t.Fatalf("status while IN_USE: code=%d body=%s", code, body)
	}

	lm.endExecute(rec.DataID)
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+rec.DataID, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("delete after execution finished: code=%d, want 200", code)
	}
}

// TestAPIBodyTooLarge はサイズ上限超過が 413 になることを確認する
func TestAPIBodyTooLarge(t *testing.T) {
	srv := newTestServer(t)
	big := bytes.Repeat([]byte{0xaa}, maxDataBytes+1)
	code, _ := doReq(t, "POST", srv.URL+"/data", big, nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized register: code=%d, want 413", code)
	}
}
