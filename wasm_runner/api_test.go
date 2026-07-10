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
	"os"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	lm := newTestManager(t, t.TempDir())
	um, err := newUserManager(lm.store)
	if err != nil {
		t.Fatalf("newUserManager: %v", err)
	}
	sb := &sandbox{execTimeout: 10 * time.Second, memLimitPages: 1024}
	srv := httptest.NewServer(newHandler(lm, sb, um))
	t.Cleanup(srv.Close)
	return srv
}

// createTestUser は POST /users でユーザを発行し、owner_id と認証ヘッダを返す
func createTestUser(t *testing.T, srvURL string) (string, map[string]string) {
	t.Helper()
	code, body := doReq(t, "POST", srvURL+"/users", nil, nil)
	if code != http.StatusCreated {
		t.Fatalf("create user: code=%d body=%s", code, body)
	}
	var u struct {
		OwnerID string `json:"owner_id"`
		APIKey  string `json:"api_key"`
	}
	if err := json.Unmarshal(body, &u); err != nil || u.OwnerID == "" || u.APIKey == "" {
		t.Fatalf("create user response invalid: %s (err=%v)", body, err)
	}
	return u.OwnerID, map[string]string{"X-API-Key": u.APIKey}
}

// execBody は POST /execute の JSON ボディを組み立てる（wasm は base64 化される）
func execBody(t *testing.T, wasm []byte, ids, args []string) []byte {
	t.Helper()
	b, err := json.Marshal(executeRequest{Wasm: wasm, Data: ids, Args: args})
	if err != nil {
		t.Fatalf("marshal execute body: %v", err)
	}
	return b
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

// TestAPILifecycleFlow は user -> register -> status -> execute -> delete -> proof の
// 一連の流れを HTTP 経由で検証する
func TestAPILifecycleFlow(t *testing.T) {
	srv := newTestServer(t)
	_, auth := createTestUser(t, srv.URL)
	data := []byte("lifecycle test data")

	// 登録（認証必須）
	code, body := doReq(t, "POST", srv.URL+"/data", data, auth)
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

	// 状態確認（認証不要）
	code, body = doReq(t, "GET", srv.URL+"/data/"+reg.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
		t.Fatalf("status: code=%d body=%s", code, body)
	}

	// 実行（no-op モジュール）
	code, body = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{reg.DataID}, nil), auth)
	if code != http.StatusOK {
		t.Fatalf("execute: code=%d body=%s", code, body)
	}

	// 削除 → 削除証明が返る
	code, certBody := doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, auth)
	if code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", code, certBody)
	}
	verifyCertificate(t, certBody, reg.DataID, data)

	// 状態は DELETED
	code, body = doReq(t, "GET", srv.URL+"/data/"+reg.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"DELETED"`) {
		t.Fatalf("status after delete: code=%d body=%s", code, body)
	}

	// 証明の再取得（監査用、認証不要）は削除時のものと一致
	code, proofBody := doReq(t, "GET", srv.URL+"/data/"+reg.DataID+"/proof", nil, nil)
	if code != http.StatusOK || !bytes.Equal(proofBody, certBody) {
		t.Fatalf("proof: code=%d body=%s", code, proofBody)
	}

	// DELETED への execute は 404（§5 不変条件3）
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{reg.DataID}, nil), auth)
	if code != http.StatusNotFound {
		t.Fatalf("execute after delete: code=%d, want 404", code)
	}

	// 再削除は 409
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, auth)
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
	_, auth := createTestUser(t, srv.URL)

	// 空ボディは登録できない
	code, _ := doReq(t, "POST", srv.URL+"/data", nil, auth)
	if code != http.StatusBadRequest {
		t.Fatalf("empty register: code=%d, want 400", code)
	}

	// 未知のIDへの操作は 404（execute/delete は認証を通した上で）
	for _, r := range []struct {
		method, path string
		headers      map[string]string
	}{
		{"POST", "/execute", auth},
		{"DELETE", "/data/d-ffffffffffffffff", auth},
		{"GET", "/data/d-ffffffffffffffff/status", nil},
		{"GET", "/data/d-ffffffffffffffff/proof", nil},
	} {
		body := []byte(nil)
		if r.method == "POST" {
			body = execBody(t, noopWasm(), []string{"d-ffffffffffffffff"}, nil)
		}
		code, _ := doReq(t, r.method, srv.URL+r.path, body, r.headers)
		if code != http.StatusNotFound {
			t.Fatalf("%s %s: code=%d, want 404", r.method, r.path, code)
		}
	}
}

// TestAPIUserAuth はユーザ認証（401）と所有者照合（403）を確認する
func TestAPIUserAuth(t *testing.T) {
	srv := newTestServer(t)
	_, authA := createTestUser(t, srv.URL)
	_, authB := createTestUser(t, srv.URL)

	// APIキー無し・無効なキーでは登録できない
	code, _ := doReq(t, "POST", srv.URL+"/data", []byte("guarded"), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("register without key: code=%d, want 401", code)
	}
	code, _ = doReq(t, "POST", srv.URL+"/data", []byte("guarded"),
		map[string]string{"X-API-Key": "ak-bogus"})
	if code != http.StatusUnauthorized {
		t.Fatalf("register with invalid key: code=%d, want 401", code)
	}

	// ユーザAがデータを登録
	code, body := doReq(t, "POST", srv.URL+"/data", []byte("guarded"), authA)
	if code != http.StatusCreated {
		t.Fatalf("register: code=%d body=%s", code, body)
	}
	var reg struct {
		DataID string `json:"data_id"`
	}
	_ = json.Unmarshal(body, &reg)

	// キー無しの execute/delete は 401
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{reg.DataID}, nil), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("execute without key: code=%d, want 401", code)
	}
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("delete without key: code=%d, want 401", code)
	}

	// 他ユーザ（B）による execute/delete は 403
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{reg.DataID}, nil), authB)
	if code != http.StatusForbidden {
		t.Fatalf("execute by other user: code=%d, want 403", code)
	}
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, authB)
	if code != http.StatusForbidden {
		t.Fatalf("delete by other user: code=%d, want 403", code)
	}

	// 所有者の混在は不可: Bのデータを混ぜたAの実行は 403（all-or-nothing）
	code, body = doReq(t, "POST", srv.URL+"/data", []byte("b's data"), authB)
	if code != http.StatusCreated {
		t.Fatalf("register by B: code=%d body=%s", code, body)
	}
	var regB struct {
		DataID string `json:"data_id"`
	}
	_ = json.Unmarshal(body, &regB)
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{reg.DataID, regB.DataID}, nil), authA)
	if code != http.StatusForbidden {
		t.Fatalf("execute with mixed owners: code=%d, want 403", code)
	}
	code, body = doReq(t, "GET", srv.URL+"/data/"+reg.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
		t.Fatalf("status after failed mixed execute: code=%d body=%s", code, body)
	}

	// Authorization: Bearer でも渡せる
	bearer := map[string]string{"Authorization": "Bearer " + authA["X-API-Key"]}
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{reg.DataID}, nil), bearer)
	if code != http.StatusOK {
		t.Fatalf("execute with bearer key: code=%d, want 200", code)
	}

	// 所有者本人は削除できる
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+reg.DataID, nil, authA)
	if code != http.StatusOK {
		t.Fatalf("delete by owner: code=%d, want 200", code)
	}
}

// TestAPIStatelessExecute はデータ指定ゼロ個（ステートレス実行、認証不要）を確認する
func TestAPIStatelessExecute(t *testing.T) {
	srv := newTestServer(t)

	code, _ := doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), nil, nil), nil)
	if code != http.StatusOK {
		t.Fatalf("stateless execute: code=%d, want 200", code)
	}

	// 壊れたバイナリは JSON エラーを返す
	code, body := doReq(t, "POST", srv.URL+"/execute", execBody(t, []byte("not wasm"), nil, nil), nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "WASM error:") {
		t.Fatalf("stateless execute invalid: code=%d body=%s", code, body)
	}
}

// TestAPIExecuteBodyValidation は execute の JSON ボディの検証を確認する
func TestAPIExecuteBodyValidation(t *testing.T) {
	srv := newTestServer(t)

	// JSON でないボディ（旧方式: 生の WASM バイナリ）は 400
	code, body := doReq(t, "POST", srv.URL+"/execute", noopWasm(), nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "invalid JSON body") {
		t.Fatalf("raw wasm body: code=%d body=%s, want 400 invalid JSON body", code, body)
	}

	// base64 として不正な wasm は 400
	code, body = doReq(t, "POST", srv.URL+"/execute", []byte(`{"wasm":"???not-base64???"}`), nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "invalid JSON body") {
		t.Fatalf("invalid base64: code=%d body=%s, want 400", code, body)
	}

	// wasm 無し・空は 400
	for _, b := range []string{`{}`, `{"wasm":""}`, `{"args":["list"]}`} {
		code, body = doReq(t, "POST", srv.URL+"/execute", []byte(b), nil)
		if code != http.StatusBadRequest || !strings.Contains(string(body), "empty wasm binary") {
			t.Fatalf("body %s: code=%d body=%s, want 400 empty wasm binary", b, code, body)
		}
	}
}

// TestAPIExecuteMultiData は複数データ指定の実行を確認する
func TestAPIExecuteMultiData(t *testing.T) {
	srv := newTestServer(t)
	_, auth := createTestUser(t, srv.URL)

	reg := func(data []byte) string {
		code, body := doReq(t, "POST", srv.URL+"/data", data, auth)
		if code != http.StatusCreated {
			t.Fatalf("register: code=%d body=%s", code, body)
		}
		var r struct {
			DataID string `json:"data_id"`
		}
		_ = json.Unmarshal(body, &r)
		return r.DataID
	}
	idA := reg([]byte("alpha\n"))
	idB := reg([]byte("beta\n"))

	// 同一IDの重複指定は 400
	code, _ := doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{idA, idA}, nil), auth)
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate ids: code=%d, want 400", code)
	}

	// 空のIDは 400
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{""}, nil), auth)
	if code != http.StatusBadRequest {
		t.Fatalf("empty id: code=%d, want 400", code)
	}

	// 存在しないIDが混ざると 404。既存データの状態は変わらない（all-or-nothing）
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{idA, "d-ffffffffffffffff"}, nil), auth)
	if code != http.StatusNotFound {
		t.Fatalf("unknown id mixed: code=%d, want 404", code)
	}
	code, body := doReq(t, "GET", srv.URL+"/data/"+idA+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
		t.Fatalf("status after failed execute: code=%d body=%s", code, body)
	}

	// 2件指定の実行が成功し、実行後は両方とも REGISTERED に戻る
	code, body = doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{idA, idB}, nil), auth)
	if code != http.StatusOK {
		t.Fatalf("execute with 2 ids: code=%d body=%s", code, body)
	}
	for _, id := range []string{idA, idB} {
		code, body = doReq(t, "GET", srv.URL+"/data/"+id+"/status", nil, nil)
		if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
			t.Fatalf("status after execute: code=%d body=%s", code, body)
		}
	}

	// readinput fixture があれば、指定順に /data/input0, input1 として見えることを内容で確認
	wasmBin, err := os.ReadFile("testdata/readinput.wasm")
	if err != nil {
		t.Skipf("testdata/readinput.wasm not found: %v", err)
	}
	code, body = doReq(t, "POST", srv.URL+"/execute", execBody(t, wasmBin, []string{idB, idA}, nil), auth)
	if code != http.StatusOK || string(body) != "beta\nalpha\n" {
		t.Fatalf("multi-input execute: code=%d body=%q, want %q", code, body, "beta\nalpha\n")
	}
}

// TestAPIExecuteArgs は JSON ボディの args が WASI argv としてモジュールに渡り、
// ライフサイクル管理（登録）を経ないことを確認する
func TestAPIExecuteArgs(t *testing.T) {
	srv := newTestServer(t)

	// ステートレス実行 + args（認証不要）
	code, body := doReq(t, "POST", srv.URL+"/execute", execBody(t, argsEchoWasm(), nil, []string{"get", "github"}), nil)
	if code != http.StatusOK || string(body) != "app.wasm\x00get\x00github\x00" {
		t.Fatalf("execute with args: code=%d body=%q", code, body)
	}

	// 空文字列の arg も argv としてそのまま渡る（data と違い 400 にしない）
	code, body = doReq(t, "POST", srv.URL+"/execute", execBody(t, argsEchoWasm(), nil, []string{""}), nil)
	if code != http.StatusOK || string(body) != "app.wasm\x00\x00" {
		t.Fatalf("execute with empty arg: code=%d body=%q", code, body)
	}

	// data と args の併用（認証必須なのは data 側の要件）
	_, auth := createTestUser(t, srv.URL)
	code, body = doReq(t, "POST", srv.URL+"/data", []byte("vault"), auth)
	if code != http.StatusCreated {
		t.Fatalf("register: code=%d body=%s", code, body)
	}
	var reg struct {
		DataID string `json:"data_id"`
	}
	_ = json.Unmarshal(body, &reg)
	code, body = doReq(t, "POST", srv.URL+"/execute", execBody(t, argsEchoWasm(), []string{reg.DataID}, []string{"list"}), auth)
	if code != http.StatusOK || string(body) != "app.wasm\x00list\x00" {
		t.Fatalf("execute with data+args: code=%d body=%q", code, body)
	}

	// 合計サイズ超過は 413
	big := strings.Repeat("a", maxArgsBytes+1)
	code, _ = doReq(t, "POST", srv.URL+"/execute", execBody(t, argsEchoWasm(), nil, []string{big}), nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized args: code=%d, want 413", code)
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
	um, err := newUserManager(lm.store)
	if err != nil {
		t.Fatalf("newUserManager: %v", err)
	}
	sb := &sandbox{execTimeout: 10 * time.Second, memLimitPages: 1024}
	srv := httptest.NewServer(newHandler(lm, sb, um))
	t.Cleanup(srv.Close)

	user, key, err := um.createUser()
	if err != nil {
		t.Fatalf("createUser: %v", err)
	}
	auth := map[string]string{"X-API-Key": key}
	rec, _ := lm.register([]byte("busy data"), user.OwnerID)

	// IN_USE 状態を直接作る（実行中の状態を模擬）
	if _, err := lm.beginExecute([]string{rec.DataID}, user.OwnerID); err != nil {
		t.Fatalf("beginExecute: %v", err)
	}

	code, _ := doReq(t, "POST", srv.URL+"/execute", execBody(t, noopWasm(), []string{rec.DataID}, nil), auth)
	if code != http.StatusConflict {
		t.Fatalf("execute while IN_USE: code=%d, want 409", code)
	}
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+rec.DataID, nil, auth)
	if code != http.StatusConflict {
		t.Fatalf("delete while IN_USE: code=%d, want 409", code)
	}
	code, body := doReq(t, "GET", srv.URL+"/data/"+rec.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"IN_USE"`) {
		t.Fatalf("status while IN_USE: code=%d body=%s", code, body)
	}

	lm.endExecute([]string{rec.DataID})
	code, _ = doReq(t, "DELETE", srv.URL+"/data/"+rec.DataID, nil, auth)
	if code != http.StatusOK {
		t.Fatalf("delete after execution finished: code=%d, want 200", code)
	}
}

// TestAPIBodyTooLarge はサイズ上限超過が 413 になることを確認する
func TestAPIBodyTooLarge(t *testing.T) {
	srv := newTestServer(t)
	_, auth := createTestUser(t, srv.URL)
	big := bytes.Repeat([]byte{0xaa}, maxDataBytes+1)
	code, _ := doReq(t, "POST", srv.URL+"/data", big, auth)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized register: code=%d, want 413", code)
	}
}
