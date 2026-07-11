package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const (
	maxDataBytes = 32 << 20 // 登録データ（base64 デコード後）の上限（リソース保護）
	maxWasmBytes = 32 << 20 // WASMバイナリ（base64 デコード後）の上限（リソース保護）
	maxArgsBytes = 8 << 10  // execute の引数（args）の合計上限（リソース保護）
	// POST /programs・POST /data の JSON ボディ上限。base64 で約 4/3 倍に膨れた
	// 32 MiB + ホワイトリスト・余白
	maxUploadBodyBytes = 44 << 20
	// POST /execute の JSON ボディ上限。WASM 本体が program_id 参照になったため、
	// ボディは小さな JSON（program_id + データID列 + args）のみ
	maxExecBodyBytes = 1 << 20
	// POST /owner・PUT /data/{id}/programs 等、小さな JSON ボディの上限
	maxSmallBodyBytes = 1 << 20
)

type server struct {
	lm   *lifecycleManager
	sb   *sandbox
	pr   *programRegistry
	om   *ownerManager
	auth *authenticator
}

// newHandler は API ルーティング（§7）を構築する。
// 生値（平文）を直接返す API は存在しない（§5 不変条件1）:
// register は書き込み専用、execute は WASM プログラムの計算結果のみを返す。
//
// 認証はすべて Ed25519 署名（auth.go）。状態を変える操作（コマンド）は
// オーナー鍵の登録（POST /owner、TOFU）が済むまですべて拒否される
func newHandler(lm *lifecycleManager, sb *sandbox, pr *programRegistry, om *ownerManager, auth *authenticator) http.Handler {
	s := &server{lm: lm, sb: sb, pr: pr, om: om, auth: auth}
	mux := http.NewServeMux()
	// ヘルスチェック --- GET /
	mux.HandleFunc("GET /{$}", s.handleHealth)
	// オーナー公開鍵の登録（初回のみ・TOFU）
	mux.HandleFunc("POST /owner", s.handleRegisterOwner)
	// プログラムレジストリ（オーナー署名必須）
	mux.HandleFunc("POST /programs", s.handleUploadProgram)
	mux.HandleFunc("DELETE /programs/{id}", s.handleDeleteProgram)
	// WASM 実行（オーナー署名必須。program_id で登録済みプログラムを参照）
	mux.HandleFunc("POST /execute", s.handleExecute)
	// ライフサイクル管理 API
	mux.HandleFunc("POST /data", s.handleRegister)
	mux.HandleFunc("PUT /data/{id}/programs", s.handleSetAllowedPrograms)
	mux.HandleFunc("DELETE /data/{id}", s.handleDelete)
	mux.HandleFunc("GET /data/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /data/{id}/proof", s.handleProof)
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	okText(w, http.StatusOK, `OK: POST /owner | POST /programs | DELETE /programs/{id} | POST /execute (JSON body: {"program_id":"p-...","data":["<id>",...],"args":["<v>",...]}; data/args optional) | POST /data | PUT /data/{id}/programs | DELETE /data/{id} | GET /data/{id}/status | GET /data/{id}/proof`)
}

// decodeJSON は JSON ボディを厳密にデコードする。未知フィールドは拒否する
// （旧 API の "wasm" フィールド等が黙って無視される事故を防ぐ）
func decodeJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// 後続のゴミ（2つ目の JSON 値など）も拒否する
	if dec.More() {
		return errors.New("unexpected trailing data after JSON body")
	}
	return nil
}

// handleRegisterOwner はオーナー公開鍵の登録（POST /owner、§3.1 案B: TOFU）。
// 認証不要だが未登録のときの初回しか受け付けず、以後は 409 で変更不可。
// 初回登録リクエストの先取りリスクへの対策は「デプロイ直後に即座に登録する」運用
func (s *server) handleRegisterOwner(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxSmallBodyBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read body: %v", err))
		return
	}
	var req struct {
		PublicKey string `json:"public_key"`
	}
	if err := decodeJSON(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	rec, err := s.om.register(req.PublicKey)
	if err != nil {
		switch {
		case errors.Is(err, errOwnerExists):
			writeJSONError(w, http.StatusConflict, err.Error())
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"public_key":    rec.PublicKey,
		"registered_at": rec.RegisteredAt,
	})
}

// authenticateCommand はコマンド（状態を変える操作）共通の前段。
// オーナー鍵が未登録なら 403（未登録状態でコマンドが通る窓を作らない、§3.1）、
// 署名が無効なら 401 を書き込み ok = false を返す。
// 成功時は署名者の公開鍵（base64）と登録済みオーナー鍵を返す。認可（署名者が
// オーナーか・記録済みアップローダか）は各ハンドラで行う
func (s *server) authenticateCommand(w http.ResponseWriter, r *http.Request, body []byte) (signer, ownerKey string, ok bool) {
	ownerKey, registered := s.om.key()
	if !registered {
		writeJSONError(w, http.StatusForbidden,
			errNoOwner.Error()+"; register the owner public key via POST /owner first")
		return "", "", false
	}
	signer, err := s.auth.authenticate(r, body)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, err.Error())
		return "", "", false
	}
	return signer, ownerKey, true
}

// requireOwner はオーナー鍵での署名を必須とするコマンドの前段。
// 成功時は登録済みオーナー鍵（＝署名者）を返す
func (s *server) requireOwner(w http.ResponseWriter, r *http.Request, body []byte) (string, bool) {
	signer, ownerKey, ok := s.authenticateCommand(w, r, body)
	if !ok {
		return "", false
	}
	if signer != ownerKey {
		writeJSONError(w, http.StatusForbidden, "signature by the registered owner key is required")
		return "", false
	}
	return ownerKey, true
}

// handleUploadProgram は WASM プログラムの事前アップロード（POST /programs、§3.2）。
// オーナー署名必須。program_id はバイナリの sha256 によるコンテンツアドレスで、
// 同一バイナリの再アップロードは冪等（新規 201 / 既存 200）
func (s *server) handleUploadProgram(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxUploadBodyBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read body: %v", err))
		return
	}
	ownerKey, ok := s.requireOwner(w, r, body)
	if !ok {
		return
	}
	var req struct {
		Wasm []byte `json:"wasm"` // WASM バイナリ（base64、必須）
	}
	if err := decodeJSON(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if len(req.Wasm) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty wasm binary")
		return
	}
	if len(req.Wasm) > maxWasmBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("wasm exceeds %d bytes", maxWasmBytes))
		return
	}
	rec, created, err := s.pr.put(req.Wasm, ownerKey)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]string{
		"program_id":  rec.ProgramID,
		"uploaded_at": rec.UploadedAt,
	})
}

// handleDeleteProgram はプログラムの削除（DELETE /programs/{id}）。
// 削除できるのはオーナーのみ（§7-Q5）。データと違い削除証明は発行しない
func (s *server) handleDeleteProgram(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxSmallBodyBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read body: %v", err))
		return
	}
	if _, ok := s.requireOwner(w, r, body); !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.pr.delete(id); err != nil {
		switch {
		case errors.Is(err, errProgramNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"program_id": id})
}

// registerRequest は POST /data の JSON ボディ
type registerRequest struct {
	Data []byte `json:"data"` // データ本体（base64、必須）
	// AllowedPrograms はこのデータに対して実行を許可する program_id（省略可）。
	// 省略・空配列＝すべて拒否（deny by default、§7-Q8）。
	// コンテンツアドレス参照のため、未アップロードのプログラムも指定できる
	AllowedPrograms []string `json:"allowed_programs"`
}

// handleRegister はデータ登録（POST /data）。任意の Ed25519 鍵による署名必須で、
// 事前のユーザ登録は不要（公開鍵の提示＋署名検証のみの自己認証的な識別、§3.3）。
// 署名者の公開鍵がアップローダとしてメタデータに記録され、以後ホワイトリスト編集・
// 削除の認可主体になる
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxUploadBodyBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read body: %v", err))
		return
	}
	signer, _, ok := s.authenticateCommand(w, r, body)
	if !ok {
		return
	}
	var req registerRequest
	if err := decodeJSON(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if len(req.Data) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty data")
		return
	}
	if len(req.Data) > maxDataBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("data exceeds %d bytes", maxDataBytes))
		return
	}
	if err := validateProgramIDs(req.AllowedPrograms); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.lm.register(req.Data, signer, req.AllowedPrograms)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"data_id":       rec.DataID,
		"registered_at": rec.CreatedAt,
	})
}

// validateProgramIDs はホワイトリスト指定の形式（p-<sha256 hex 64桁>）と重複を検査する
func validateProgramIDs(ids []string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !isProgramID(id) {
			return fmt.Errorf("invalid program id (want p-<sha256 hex>): %q", id)
		}
		if seen[id] {
			return errors.New("duplicate program id: " + id)
		}
		seen[id] = true
	}
	return nil
}

// handleSetAllowedPrograms はホワイトリストの全置換（PUT /data/{id}/programs、§3.4）。
// 編集できるのは当該データに記録済みのアップローダ鍵の署名者のみ。
// オーナー鍵の署名では編集できない（アップローダ専権）
func (s *server) handleSetAllowedPrograms(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxSmallBodyBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read body: %v", err))
		return
	}
	signer, _, ok := s.authenticateCommand(w, r, body)
	if !ok {
		return
	}
	var req struct {
		AllowedPrograms []string `json:"allowed_programs"`
	}
	if err := decodeJSON(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if err := validateProgramIDs(req.AllowedPrograms); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	rec, err := s.lm.setAllowedPrograms(id, signer, req.AllowedPrograms)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errDeleted):
			// 削除済みデータのホワイトリストは編集不可（実行され得ないため意味を持たない）
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errBusy):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errForbidden):
			writeJSONError(w, http.StatusForbidden,
				"signature by the uploader key recorded for this data is required")
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data_id":          rec.DataID,
		"allowed_programs": rec.AllowedPrograms,
	})
}

// executeRequest は POST /execute の JSON ボディ
type executeRequest struct {
	ProgramID string   `json:"program_id"` // 事前アップロード済みプログラムのID（必須）
	Data      []string `json:"data"`       // 使用する登録済みデータのID（0個以上、省略可）
	Args      []string `json:"args"`       // WASI argv（0個以上、省略可）
}

// handleExecute は WASM 実行（POST /execute）。ボディ＝JSON（executeRequest）。
// オーナー鍵の署名必須（データ指定なしのステートレス実行も含めて統一、§7-Q4）。
// data の指定順の i 番目が読み取り専用ファイル /data/input<i> として WASM から
// 見える。args は WASI argv として指定順に argv[1] 以降として渡る。args は
// 使い捨ての実行パラメータであり、ライフサイクル管理（登録・削除証明）の対象に
// ならない。実行内容はすべてボディで渡すため、URL（アクセスログ・プロキシ等に
// 残る）に実行内容が乗ることはない。
// 指定した各データは program_id がホワイトリストに含まれていなければならず、
// 1件でも外れていれば 403（応答にどのデータが拒否したかを含める、§3.4）。
// 応答＝実行結果（stdout、stderrがあれば併記）
func (s *server) handleExecute(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxExecBodyBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read body: %v", err))
		return
	}
	if _, ok := s.requireOwner(w, r, body); !ok {
		return
	}
	var req executeRequest
	if err := decodeJSON(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if req.ProgramID == "" {
		writeJSONError(w, http.StatusBadRequest, "empty program_id")
		return
	}
	argsLen := 0
	for _, a := range req.Args {
		argsLen += len(a)
	}
	if argsLen > maxArgsBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("args exceed %d bytes total", maxArgsBytes))
		return
	}

	ids := req.Data
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			writeJSONError(w, http.StatusBadRequest, "empty data id")
			return
		}
		if seen[id] {
			writeJSONError(w, http.StatusBadRequest, "duplicate data id: "+id)
			return
		}
		seen[id] = true
	}

	// プログラムの取得（未知の program_id は 404）。取得後に削除されても、
	// この実行は取得時点のバイナリのスナップショットで続行される
	wasmBin, err := s.pr.get(req.ProgramID)
	if err != nil {
		switch {
		case errors.Is(err, errProgramNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// 指定された全データを原子的に IN_USE にする
	// （全データのホワイトリストに program_id が含まれることを照合）
	inputs, err := s.lm.beginExecute(ids, req.ProgramID)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errDeleted):
			// DELETED への execute は 404（§5 不変条件3）
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errBusy):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errProgramNotAllowed):
			writeJSONError(w, http.StatusForbidden, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	defer s.lm.endExecute(ids)

	out, err := s.sb.run(r.Context(), wasmBin, inputs, req.Args)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("WASM error: %v", err))
		return
	}
	okText(w, http.StatusOK, out)
}

// handleDelete はデータ削除（DELETE /data/{id}）。オーナー鍵または当該データに
// 記録済みのアップローダ鍵の署名必須（§7-Q3: 両方可）。応答＝削除証明（JSON）
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, maxSmallBodyBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read body: %v", err))
		return
	}
	signer, ownerKey, ok := s.authenticateCommand(w, r, body)
	if !ok {
		return
	}
	id := r.PathValue("id")
	cert, err := s.lm.delete(id, signer, signer == ownerKey)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errDeleted):
			writeJSONError(w, http.StatusConflict,
				fmt.Sprintf("already deleted; deletion certificate is available at GET /data/%s/proof", id))
		case errors.Is(err, errBusy):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errForbidden):
			writeJSONError(w, http.StatusForbidden,
				"signature by the owner key or the uploader key recorded for this data is required")
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeRawJSON(w, http.StatusOK, cert)
}

// handleStatus は現在の状態を返す（GET /data/{id}/status）。生データは返さない
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	info, err := s.lm.status(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleProof は削除証明の再取得（GET /data/{id}/proof、監査用）
func (s *server) handleProof(w http.ResponseWriter, r *http.Request) {
	cert, err := s.lm.proof(r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errNotDeleted):
			writeJSONError(w, http.StatusConflict, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeRawJSON(w, http.StatusOK, cert)
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
}

func bodyErrStatus(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func okText(w http.ResponseWriter, code int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(s))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRawJSON(w http.ResponseWriter, code int, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
