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

// testServer は HTTP レイヤのテスト用サーバ。owner はヘルパで登録済みのオーナー鍵
// （newTestServerNoOwner の場合は nil）
type testServer struct {
	*httptest.Server
	lm    *lifecycleManager
	owner *testKey
}

func newTestServerNoOwner(t *testing.T) *testServer {
	t.Helper()
	dir := t.TempDir()
	st, err := newStore(dir)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	om, err := newOwnerManager(st)
	if err != nil {
		t.Fatalf("newOwnerManager: %v", err)
	}
	preg, err := newProgramRegistry(st)
	if err != nil {
		t.Fatalf("newProgramRegistry: %v", err)
	}
	lm, err := newLifecycleManager(st, newProver())
	if err != nil {
		t.Fatalf("newLifecycleManager: %v", err)
	}
	sb := &sandbox{execTimeout: 10 * time.Second, memLimitPages: 1024}
	srv := httptest.NewServer(newHandler(lm, sb, preg, om, newAuthenticator(5*time.Minute)))
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, lm: lm}
}

// newTestServer はオーナー鍵を生成して POST /owner で登録済みのサーバを返す
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := newTestServerNoOwner(t)
	ts.owner = genTestKey(t)
	code, body := doReq(t, "POST", ts.URL+"/owner",
		jsonBody(t, map[string]string{"public_key": ts.owner.pubB64()}), nil)
	if code != http.StatusCreated {
		t.Fatalf("register owner: code=%d body=%s", code, body)
	}
	return ts
}

// signedReq は k で署名した（X-Public-Key / X-Signature / X-Timestamp 付き）
// リクエストを送る
func (ts *testServer) signedReq(t *testing.T, k *testKey, method, path string, body []byte) (int, []byte) {
	t.Helper()
	return doReq(t, method, ts.URL+path, body, k.headers(method, path, body))
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

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

// execBody は POST /execute の JSON ボディを組み立てる
func execBody(t *testing.T, programID string, ids, args []string) []byte {
	t.Helper()
	return jsonBody(t, executeRequest{ProgramID: programID, Data: ids, Args: args})
}

// programBody は POST /programs の JSON ボディ（wasm は base64 化される）
func programBody(t *testing.T, wasm []byte) []byte {
	t.Helper()
	return jsonBody(t, map[string][]byte{"wasm": wasm})
}

// dataBody は POST /data の JSON ボディ
func dataBody(t *testing.T, data []byte, programs []string) []byte {
	t.Helper()
	return jsonBody(t, registerRequest{Data: data, AllowedPrograms: programs})
}

// uploadProgram はオーナー署名でプログラムを登録し、program_id を返す
func (ts *testServer) uploadProgram(t *testing.T, wasm []byte) string {
	t.Helper()
	code, body := ts.signedReq(t, ts.owner, "POST", "/programs", programBody(t, wasm))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("upload program: code=%d body=%s", code, body)
	}
	var res struct {
		ProgramID string `json:"program_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.ProgramID == "" {
		t.Fatalf("upload program response invalid: %s (err=%v)", body, err)
	}
	return res.ProgramID
}

// uploadData は k の署名でデータを登録し、data_id を返す
func (ts *testServer) uploadData(t *testing.T, k *testKey, data []byte, programs []string) string {
	t.Helper()
	code, body := ts.signedReq(t, k, "POST", "/data", dataBody(t, data, programs))
	if code != http.StatusCreated {
		t.Fatalf("upload data: code=%d body=%s", code, body)
	}
	var res struct {
		DataID string `json:"data_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.DataID == "" {
		t.Fatalf("upload data response invalid: %s (err=%v)", body, err)
	}
	return res.DataID
}

// TestAPILifecycleFlow は owner 登録 -> program 登録 -> data 登録 -> status ->
// execute -> delete -> proof の一連の流れを HTTP 経由で検証する
func TestAPILifecycleFlow(t *testing.T) {
	ts := newTestServer(t)
	uploader := genTestKey(t)
	data := []byte("lifecycle test data")

	pid := ts.uploadProgram(t, noopWasm())
	id := ts.uploadData(t, uploader, data, []string{pid})

	// 状態確認（認証不要）
	code, body := doReq(t, "GET", ts.URL+"/data/"+id+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
		t.Fatalf("status: code=%d body=%s", code, body)
	}

	// 実行（オーナー署名）
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{id}, nil))
	if code != http.StatusOK {
		t.Fatalf("execute: code=%d body=%s", code, body)
	}

	// 削除（アップローダ署名）→ 削除証明が返る
	code, certBody := ts.signedReq(t, uploader, "DELETE", "/data/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", code, certBody)
	}
	verifyCertificate(t, certBody, id, data)

	// 状態は DELETED
	code, body = doReq(t, "GET", ts.URL+"/data/"+id+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"DELETED"`) {
		t.Fatalf("status after delete: code=%d body=%s", code, body)
	}

	// 証明の再取得（監査用、認証不要）は削除時のものと一致
	code, proofBody := doReq(t, "GET", ts.URL+"/data/"+id+"/proof", nil, nil)
	if code != http.StatusOK || !bytes.Equal(proofBody, certBody) {
		t.Fatalf("proof: code=%d body=%s", code, proofBody)
	}

	// DELETED への execute は 404（§5 不変条件3）
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{id}, nil))
	if code != http.StatusNotFound {
		t.Fatalf("execute after delete: code=%d, want 404", code)
	}

	// 再削除は 409
	code, _ = ts.signedReq(t, uploader, "DELETE", "/data/"+id, nil)
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

// TestAPIOwnerTOFU はオーナー鍵の TOFU 登録（§3.1 案B）を確認する:
// 未登録時はコマンドが全拒否され、初回登録のみ受け付け、以後は変更不可
func TestAPIOwnerTOFU(t *testing.T) {
	ts := newTestServerNoOwner(t)
	k := genTestKey(t)

	// 認証不要 API は未登録でも生きている
	code, body := doReq(t, "GET", ts.URL+"/", nil, nil)
	if code != http.StatusOK || !strings.HasPrefix(string(body), "OK") {
		t.Fatalf("health before registration: code=%d body=%s", code, body)
	}
	code, _ = doReq(t, "GET", ts.URL+"/data/d-ffffffffffffffff/status", nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("status before registration: code=%d, want 404", code)
	}

	// 未登録時のコマンドはすべて 403（正当な署名付きでも、署名なしでも）
	pid := programID(noopWasm())
	commands := []struct {
		method, path string
		body         []byte
	}{
		{"POST", "/programs", programBody(t, noopWasm())},
		{"DELETE", "/programs/" + pid, nil},
		{"POST", "/execute", execBody(t, pid, nil, nil)},
		{"POST", "/data", dataBody(t, []byte("x"), nil)},
		{"PUT", "/data/d-ffffffffffffffff/programs", jsonBody(t, map[string][]string{"allowed_programs": nil})},
		{"DELETE", "/data/d-ffffffffffffffff", nil},
	}
	for _, c := range commands {
		code, body := ts.signedReq(t, k, c.method, c.path, c.body)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s before registration (signed): code=%d body=%s, want 403", c.method, c.path, code, body)
		}
		code, _ = doReq(t, c.method, ts.URL+c.path, c.body, nil)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s before registration (unsigned): code=%d, want 403", c.method, c.path, code)
		}
	}

	// 不正な公開鍵の登録は 400
	for _, bad := range []string{`{}`, `{"public_key":""}`, `{"public_key":"???"}`, `{"public_key":"c2hvcnQ="}`} {
		code, _ := doReq(t, "POST", ts.URL+"/owner", []byte(bad), nil)
		if code != http.StatusBadRequest {
			t.Fatalf("register owner %s: code=%d, want 400", bad, code)
		}
	}

	// 初回登録は 201
	code, body = doReq(t, "POST", ts.URL+"/owner", jsonBody(t, map[string]string{"public_key": k.pubB64()}), nil)
	if code != http.StatusCreated || !strings.Contains(string(body), k.pubB64()) {
		t.Fatalf("register owner: code=%d body=%s", code, body)
	}

	// 二重登録は同じ鍵でも別鍵でも 409（TOFU: 以後変更不可）
	for _, key := range []string{k.pubB64(), genTestKey(t).pubB64()} {
		code, _ = doReq(t, "POST", ts.URL+"/owner", jsonBody(t, map[string]string{"public_key": key}), nil)
		if code != http.StatusConflict {
			t.Fatalf("re-register owner: code=%d, want 409", code)
		}
	}

	// 登録後はオーナー鍵のコマンドが通る
	ts.owner = k
	if got := ts.uploadProgram(t, noopWasm()); got != pid {
		t.Fatalf("program id = %s, want %s", got, pid)
	}
	code, body = ts.signedReq(t, k, "POST", "/execute", execBody(t, pid, nil, nil))
	if code != http.StatusOK {
		t.Fatalf("execute after registration: code=%d body=%s", code, body)
	}
}

// TestAPIAuth は HTTP レイヤの署名認証（401）と認可（403）を確認する
func TestAPIAuth(t *testing.T) {
	ts := newTestServer(t)
	other := genTestKey(t)

	// 署名ヘッダ無しは 401
	code, _ := doReq(t, "POST", ts.URL+"/programs", programBody(t, noopWasm()), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no signature: code=%d, want 401", code)
	}

	// ボディの改ざん（署名時と異なるボディ）は 401
	h := ts.owner.headers("POST", "/programs", programBody(t, noopWasm()))
	code, _ = doReq(t, "POST", ts.URL+"/programs", programBody(t, loopWasm()), h)
	if code != http.StatusUnauthorized {
		t.Fatalf("tampered body: code=%d, want 401", code)
	}

	// 別パス用の署名の流用は 401（署名は METHOD/PATH に束縛される）
	h = ts.owner.headers("POST", "/execute", programBody(t, noopWasm()))
	code, _ = doReq(t, "POST", ts.URL+"/programs", programBody(t, noopWasm()), h)
	if code != http.StatusUnauthorized {
		t.Fatalf("cross-path signature: code=%d, want 401", code)
	}

	// 期限切れタイムスタンプは 401（署名自体は正当でも）
	staleTS := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	body := programBody(t, noopWasm())
	staleSig := ed25519.Sign(ts.owner.priv, signedMessage("POST", "/programs", staleTS, body))
	code, _ = doReq(t, "POST", ts.URL+"/programs", body, map[string]string{
		headerPublicKey: ts.owner.pubB64(),
		headerSignature: base64.StdEncoding.EncodeToString(staleSig),
		headerTimestamp: staleTS,
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("stale timestamp: code=%d, want 401", code)
	}

	// リプレイ（同一署名の再送）は 401。1回目は正当に通る
	h = ts.owner.headers("POST", "/programs", body)
	code, _ = doReq(t, "POST", ts.URL+"/programs", body, h)
	if code != http.StatusCreated {
		t.Fatalf("first request: code=%d, want 201", code)
	}
	code, _ = doReq(t, "POST", ts.URL+"/programs", body, h)
	if code != http.StatusUnauthorized {
		t.Fatalf("replayed request: code=%d, want 401", code)
	}

	// オーナー以外の鍵によるオーナー専用コマンドは 403（署名は正当）
	pid := programID(noopWasm())
	for _, c := range []struct {
		method, path string
		body         []byte
	}{
		{"POST", "/programs", programBody(t, noopWasm())},
		{"DELETE", "/programs/" + pid, nil},
		{"POST", "/execute", execBody(t, pid, nil, nil)},
	} {
		code, body := ts.signedReq(t, other, c.method, c.path, c.body)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s by non-owner: code=%d body=%s, want 403", c.method, c.path, code, body)
		}
	}
}

// TestAPIProgramRegistry はプログラムの登録・冪等性・削除を HTTP 経由で確認する
func TestAPIProgramRegistry(t *testing.T) {
	ts := newTestServer(t)
	wasm := noopWasm()
	wantID := programID(wasm)

	// 新規登録は 201。program_id はクライアント側でも計算できるコンテンツアドレス
	code, body := ts.signedReq(t, ts.owner, "POST", "/programs", programBody(t, wasm))
	if code != http.StatusCreated || !strings.Contains(string(body), wantID) {
		t.Fatalf("upload: code=%d body=%s, want 201 with %s", code, body, wantID)
	}

	// 同一バイナリの再アップロードは冪等（200・同じ ID）
	code, body = ts.signedReq(t, ts.owner, "POST", "/programs", programBody(t, wasm))
	if code != http.StatusOK || !strings.Contains(string(body), wantID) {
		t.Fatalf("re-upload: code=%d body=%s, want 200 with %s", code, body, wantID)
	}

	// wasm 無し・空は 400
	for _, b := range []string{`{}`, `{"wasm":""}`} {
		code, _ = ts.signedReq(t, ts.owner, "POST", "/programs", []byte(b))
		if code != http.StatusBadRequest {
			t.Fatalf("upload %s: code=%d, want 400", b, code)
		}
	}

	// 削除（オーナーのみ）。削除後の execute は 404、再アップロードは 201 で同じ ID
	code, _ = ts.signedReq(t, ts.owner, "DELETE", "/programs/"+wantID, nil)
	if code != http.StatusOK {
		t.Fatalf("delete program: code=%d, want 200", code)
	}
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, wantID, nil, nil))
	if code != http.StatusNotFound {
		t.Fatalf("execute deleted program: code=%d, want 404", code)
	}
	code, _ = ts.signedReq(t, ts.owner, "DELETE", "/programs/"+wantID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("delete unknown program: code=%d, want 404", code)
	}
	code, body = ts.signedReq(t, ts.owner, "POST", "/programs", programBody(t, wasm))
	if code != http.StatusCreated || !strings.Contains(string(body), wantID) {
		t.Fatalf("re-upload after delete: code=%d body=%s", code, body)
	}
}

// TestAPIExecuteValidation は execute の JSON ボディの検証を確認する
func TestAPIExecuteValidation(t *testing.T) {
	ts := newTestServer(t)
	pid := ts.uploadProgram(t, noopWasm())

	// JSON でないボディ（旧方式: 生の WASM バイナリ）は 400
	code, body := ts.signedReq(t, ts.owner, "POST", "/execute", noopWasm())
	if code != http.StatusBadRequest || !strings.Contains(string(body), "invalid JSON body") {
		t.Fatalf("raw wasm body: code=%d body=%s, want 400 invalid JSON body", code, body)
	}

	// 旧 API の wasm フィールドは未知フィールドとして拒否される（§6 互換性）
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute",
		[]byte(`{"wasm":"AGFzbQEAAAA=","data":[]}`))
	if code != http.StatusBadRequest || !strings.Contains(string(body), "wasm") {
		t.Fatalf("legacy wasm field: code=%d body=%s, want 400 mentioning wasm", code, body)
	}

	// program_id 無し・空は 400
	for _, b := range []string{`{}`, `{"program_id":""}`, `{"args":["x"]}`} {
		code, body = ts.signedReq(t, ts.owner, "POST", "/execute", []byte(b))
		if code != http.StatusBadRequest || !strings.Contains(string(body), "empty program_id") {
			t.Fatalf("body %s: code=%d body=%s, want 400 empty program_id", b, code, body)
		}
	}

	// 未知の program_id は 404
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute",
		execBody(t, "p-ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", nil, nil))
	if code != http.StatusNotFound {
		t.Fatalf("unknown program: code=%d, want 404", code)
	}

	// 空のデータID・同一IDの重複指定は 400
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{""}, nil))
	if code != http.StatusBadRequest {
		t.Fatalf("empty data id: code=%d, want 400", code)
	}
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{"d-1", "d-1"}, nil))
	if code != http.StatusBadRequest {
		t.Fatalf("duplicate data ids: code=%d, want 400", code)
	}

	// WASM として不正なプログラムは実行時に 400
	junk := ts.uploadProgram(t, []byte("not wasm"))
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, junk, nil, nil))
	if code != http.StatusBadRequest || !strings.Contains(string(body), "WASM error:") {
		t.Fatalf("invalid wasm execute: code=%d body=%s", code, body)
	}
}

// TestAPIExecuteMultiData は複数データ指定の実行とホワイトリスト照合を確認する
func TestAPIExecuteMultiData(t *testing.T) {
	ts := newTestServer(t)
	uploader := genTestKey(t)

	noopID := ts.uploadProgram(t, noopWasm())
	wasmBin, err := os.ReadFile("testdata/readinput.wasm")
	if err != nil {
		t.Skipf("testdata/readinput.wasm not found: %v", err)
	}
	riID := ts.uploadProgram(t, wasmBin)

	// A は両プログラムを許可、B は readinput のみ許可
	idA := ts.uploadData(t, uploader, []byte("alpha\n"), []string{noopID, riID})
	idB := ts.uploadData(t, uploader, []byte("beta\n"), []string{riID})

	// 指定順に /data/input0, input1 として見える
	code, body := ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, riID, []string{idB, idA}, nil))
	if code != http.StatusOK || string(body) != "beta\nalpha\n" {
		t.Fatalf("multi-input execute: code=%d body=%q, want %q", code, body, "beta\nalpha\n")
	}

	// ホワイトリスト外のデータが1件でも混ざれば 403 で、応答に対象IDが含まれ、
	// どのデータの状態も変わらない（all-or-nothing、§3.4）
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, noopID, []string{idA, idB}, nil))
	if code != http.StatusForbidden || !strings.Contains(string(body), idB) {
		t.Fatalf("not-allowed mixed: code=%d body=%s, want 403 mentioning %s", code, body, idB)
	}
	for _, id := range []string{idA, idB} {
		code, body = doReq(t, "GET", ts.URL+"/data/"+id+"/status", nil, nil)
		if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
			t.Fatalf("status after rejected execute: code=%d body=%s", code, body)
		}
	}

	// 許可されている組み合わせは実行でき、実行後は REGISTERED に戻る
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, noopID, []string{idA}, nil))
	if code != http.StatusOK {
		t.Fatalf("allowed execute: code=%d body=%s", code, body)
	}
	code, body = doReq(t, "GET", ts.URL+"/data/"+idA+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
		t.Fatalf("status after execute: code=%d body=%s", code, body)
	}

	// 存在しないIDが混ざると 404。既存データの状態は変わらない（all-or-nothing）
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, riID, []string{idA, "d-ffffffffffffffff"}, nil))
	if code != http.StatusNotFound {
		t.Fatalf("unknown id mixed: code=%d, want 404", code)
	}
	code, body = doReq(t, "GET", ts.URL+"/data/"+idA+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"REGISTERED"`) {
		t.Fatalf("status after failed execute: code=%d body=%s", code, body)
	}
}

// TestAPIWhitelistEdit はホワイトリスト編集 API（PUT /data/{id}/programs、§3.4）を
// 確認する: 全置換・アップローダ専権（オーナー鍵でも不可）・deny by default
func TestAPIWhitelistEdit(t *testing.T) {
	ts := newTestServer(t)
	uploader := genTestKey(t)
	pid := ts.uploadProgram(t, noopWasm())

	// allowed_programs 未指定の登録は空リスト＝すべて拒否（deny by default、§7-Q8）
	id := ts.uploadData(t, uploader, []byte("guarded"), nil)
	code, body := ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{id}, nil))
	if code != http.StatusForbidden {
		t.Fatalf("execute with empty whitelist: code=%d body=%s, want 403", code, body)
	}

	putBody := jsonBody(t, map[string][]string{"allowed_programs": {pid}})

	// オーナー鍵・無関係な鍵による編集は 403（アップローダ専権）
	for name, k := range map[string]*testKey{"owner": ts.owner, "third party": genTestKey(t)} {
		code, body = ts.signedReq(t, k, "PUT", "/data/"+id+"/programs", putBody)
		if code != http.StatusForbidden {
			t.Fatalf("whitelist edit by %s: code=%d body=%s, want 403", name, code, body)
		}
	}

	// アップローダによる編集（全置換）で実行が許可される
	code, body = ts.signedReq(t, uploader, "PUT", "/data/"+id+"/programs", putBody)
	if code != http.StatusOK || !strings.Contains(string(body), pid) {
		t.Fatalf("whitelist edit: code=%d body=%s", code, body)
	}
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{id}, nil))
	if code != http.StatusOK {
		t.Fatalf("execute after whitelist edit: code=%d body=%s", code, body)
	}

	// 空配列で「すべて拒否」に戻せる
	code, _ = ts.signedReq(t, uploader, "PUT", "/data/"+id+"/programs",
		jsonBody(t, map[string][]string{"allowed_programs": {}}))
	if code != http.StatusOK {
		t.Fatalf("whitelist clear: code=%d, want 200", code)
	}
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{id}, nil))
	if code != http.StatusForbidden {
		t.Fatalf("execute after whitelist clear: code=%d, want 403", code)
	}

	// 形式不正・重複した program_id は 400
	for _, b := range []string{
		`{"allowed_programs":["not-a-program-id"]}`,
		`{"allowed_programs":["` + pid + `","` + pid + `"]}`,
	} {
		code, _ = ts.signedReq(t, uploader, "PUT", "/data/"+id+"/programs", []byte(b))
		if code != http.StatusBadRequest {
			t.Fatalf("whitelist edit %s: code=%d, want 400", b, code)
		}
	}

	// 未知のIDは 404、削除済みデータの編集も 404
	code, _ = ts.signedReq(t, uploader, "PUT", "/data/d-ffffffffffffffff/programs", putBody)
	if code != http.StatusNotFound {
		t.Fatalf("whitelist edit unknown id: code=%d, want 404", code)
	}
	code, _ = ts.signedReq(t, uploader, "DELETE", "/data/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("delete: code=%d, want 200", code)
	}
	code, _ = ts.signedReq(t, uploader, "PUT", "/data/"+id+"/programs", putBody)
	if code != http.StatusNotFound {
		t.Fatalf("whitelist edit after delete: code=%d, want 404", code)
	}
}

// TestAPIDeleteAuthorization は DELETE /data/{id} の認可（§7-Q3: オーナーまたは
// 記録済みアップローダ）を確認する
func TestAPIDeleteAuthorization(t *testing.T) {
	ts := newTestServer(t)
	uploader := genTestKey(t)

	// 無関係な鍵による削除は 403
	idA := ts.uploadData(t, uploader, []byte("data-a"), nil)
	code, _ := ts.signedReq(t, genTestKey(t), "DELETE", "/data/"+idA, nil)
	if code != http.StatusForbidden {
		t.Fatalf("delete by unrelated key: code=%d, want 403", code)
	}

	// オーナー鍵による削除は成功する（アップローダでなくても）
	code, cert := ts.signedReq(t, ts.owner, "DELETE", "/data/"+idA, nil)
	if code != http.StatusOK {
		t.Fatalf("delete by owner: code=%d body=%s", code, cert)
	}
	verifyCertificate(t, cert, idA, []byte("data-a"))

	// アップローダ鍵による削除も成功する
	idB := ts.uploadData(t, uploader, []byte("data-b"), nil)
	code, cert = ts.signedReq(t, uploader, "DELETE", "/data/"+idB, nil)
	if code != http.StatusOK {
		t.Fatalf("delete by uploader: code=%d body=%s", code, cert)
	}
	verifyCertificate(t, cert, idB, []byte("data-b"))
}

// TestAPIRegisterValidation は POST /data のボディ検証を確認する
func TestAPIRegisterValidation(t *testing.T) {
	ts := newTestServer(t)
	uploader := genTestKey(t)

	pid := programID(noopWasm())
	for name, b := range map[string]string{
		"non-JSON body":        "raw data",
		"empty body":           `{}`,
		"empty data":           `{"data":""}`,
		"invalid base64":       `{"data":"???"}`,
		"legacy unknown field": `{"data":"eA==","owner_id":"u-1"}`,
		"invalid program id":   `{"data":"eA==","allowed_programs":["x"]}`,
		"duplicate program id": `{"data":"eA==","allowed_programs":["` + pid + `","` + pid + `"]}`,
	} {
		code, _ := ts.signedReq(t, uploader, "POST", "/data", []byte(b))
		if code != http.StatusBadRequest {
			t.Fatalf("register %s: code=%d, want 400", name, code)
		}
	}

	// 未知のIDへの操作は 404（署名を通した上で）
	for _, r := range []struct{ method, path string }{
		{"DELETE", "/data/d-ffffffffffffffff"},
		{"GET", "/data/d-ffffffffffffffff/status"},
		{"GET", "/data/d-ffffffffffffffff/proof"},
	} {
		var code int
		if r.method == "GET" {
			code, _ = doReq(t, r.method, ts.URL+r.path, nil, nil)
		} else {
			code, _ = ts.signedReq(t, uploader, r.method, r.path, nil)
		}
		if code != http.StatusNotFound {
			t.Fatalf("%s %s: code=%d, want 404", r.method, r.path, code)
		}
	}
}

// TestAPIStatelessExecute はデータ指定ゼロ個（ステートレス実行）を確認する。
// ステートレス実行も含めてオーナー署名必須（§7-Q4）
func TestAPIStatelessExecute(t *testing.T) {
	ts := newTestServer(t)
	pid := ts.uploadProgram(t, noopWasm())

	// オーナー署名があれば成功
	code, _ := ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, nil, nil))
	if code != http.StatusOK {
		t.Fatalf("stateless execute: code=%d, want 200", code)
	}

	// 署名なしは 401、オーナー以外の鍵は 403
	code, _ = doReq(t, "POST", ts.URL+"/execute", execBody(t, pid, nil, nil), nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("stateless execute unsigned: code=%d, want 401", code)
	}
	code, _ = ts.signedReq(t, genTestKey(t), "POST", "/execute", execBody(t, pid, nil, nil))
	if code != http.StatusForbidden {
		t.Fatalf("stateless execute by non-owner: code=%d, want 403", code)
	}
}

// TestAPIExecuteArgs は JSON ボディの args が WASI argv としてモジュールに渡り、
// ライフサイクル管理（登録）を経ないことを確認する
func TestAPIExecuteArgs(t *testing.T) {
	ts := newTestServer(t)
	uploader := genTestKey(t)
	echoID := ts.uploadProgram(t, argsEchoWasm())

	// ステートレス実行 + args
	code, body := ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, echoID, nil, []string{"get", "github"}))
	if code != http.StatusOK || string(body) != "app.wasm\x00get\x00github\x00" {
		t.Fatalf("execute with args: code=%d body=%q", code, body)
	}

	// 空文字列の arg も argv としてそのまま渡る（data と違い 400 にしない）
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, echoID, nil, []string{""}))
	if code != http.StatusOK || string(body) != "app.wasm\x00\x00" {
		t.Fatalf("execute with empty arg: code=%d body=%q", code, body)
	}

	// data と args の併用
	id := ts.uploadData(t, uploader, []byte("vault"), []string{echoID})
	code, body = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, echoID, []string{id}, []string{"list"}))
	if code != http.StatusOK || string(body) != "app.wasm\x00list\x00" {
		t.Fatalf("execute with data+args: code=%d body=%q", code, body)
	}

	// 合計サイズ超過は 413
	big := strings.Repeat("a", maxArgsBytes+1)
	code, _ = ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, echoID, nil, []string{big}))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized args: code=%d, want 413", code)
	}
}

func TestAPIHealth(t *testing.T) {
	ts := newTestServer(t)
	code, body := doReq(t, "GET", ts.URL+"/", nil, nil)
	if code != http.StatusOK || !strings.HasPrefix(string(body), "OK") {
		t.Fatalf("health: code=%d body=%s", code, body)
	}
}

// TestAPIExecuteBusyConflict は実行中のデータへの競合操作が 409 になることを確認する
func TestAPIExecuteBusyConflict(t *testing.T) {
	ts := newTestServer(t)
	pid := ts.uploadProgram(t, noopWasm())

	// IN_USE 状態を直接作る（実行中の状態を模擬）
	rec, err := ts.lm.register([]byte("busy data"), testUploader, []string{pid})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := ts.lm.beginExecute([]string{rec.DataID}, pid); err != nil {
		t.Fatalf("beginExecute: %v", err)
	}

	code, _ := ts.signedReq(t, ts.owner, "POST", "/execute", execBody(t, pid, []string{rec.DataID}, nil))
	if code != http.StatusConflict {
		t.Fatalf("execute while IN_USE: code=%d, want 409", code)
	}
	code, _ = ts.signedReq(t, ts.owner, "DELETE", "/data/"+rec.DataID, nil)
	if code != http.StatusConflict {
		t.Fatalf("delete while IN_USE: code=%d, want 409", code)
	}
	code, body := doReq(t, "GET", ts.URL+"/data/"+rec.DataID+"/status", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"IN_USE"`) {
		t.Fatalf("status while IN_USE: code=%d body=%s", code, body)
	}

	ts.lm.endExecute([]string{rec.DataID})
	code, _ = ts.signedReq(t, ts.owner, "DELETE", "/data/"+rec.DataID, nil)
	if code != http.StatusOK {
		t.Fatalf("delete after execution finished: code=%d, want 200", code)
	}
}

// TestAPIBodyTooLarge はサイズ上限超過が 413 になることを確認する
func TestAPIBodyTooLarge(t *testing.T) {
	ts := newTestServer(t)
	uploader := genTestKey(t)

	// base64 デコード後のデータが maxDataBytes を超えると 413
	big := bytes.Repeat([]byte{0xaa}, maxDataBytes+1)
	code, _ := ts.signedReq(t, uploader, "POST", "/data", dataBody(t, big, nil))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized register: code=%d, want 413", code)
	}

	// base64 デコード後の wasm が maxWasmBytes を超えると 413
	code, _ = ts.signedReq(t, ts.owner, "POST", "/programs", programBody(t, big))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized program: code=%d, want 413", code)
	}
}
