package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"encoding/json"
)

const (
	maxDataBytes = 32 << 20 // 登録データの上限（リソース保護）
	maxWasmBytes = 32 << 20 // WASMバイナリの上限（リソース保護）
)

type server struct {
	lm *lifecycleManager
	sb *sandbox
}

// newHandler は API ルーティング（§7）を構築する。
// 生値（平文）を直接返す API は存在しない（§5 不変条件1）:
// register は書き込み専用、execute は WASM プログラムの計算結果のみを返す
func newHandler(lm *lifecycleManager, sb *sandbox) http.Handler {
	s := &server{lm: lm, sb: sb}
	mux := http.NewServeMux()
	// ヘルスチェック --- GET /
	mux.HandleFunc("GET /{$}", s.handleHealth)
	// ステートレス実行（既存、ライフサイクル管理・削除証明の対象外。動作確認用）
	mux.HandleFunc("POST /execute-wasm", s.handleStatelessExecute)
	// ライフサイクル管理 API
	mux.HandleFunc("POST /data", s.handleRegister)
	mux.HandleFunc("POST /data/{id}/execute", s.handleExecute)
	mux.HandleFunc("DELETE /data/{id}", s.handleDelete)
	mux.HandleFunc("GET /data/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /data/{id}/proof", s.handleProof)
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	okText(w, http.StatusOK, "OK: POST /execute-wasm (stateless) | POST /data | POST /data/{id}/execute | DELETE /data/{id} | GET /data/{id}/status | GET /data/{id}/proof")
}

// handleStatelessExecute は既存の POST /execute-wasm。ライフサイクル管理対象外だが、
// タイムアウト・メモリ上限などの実行制約（§8）は同様に適用される
func (s *server) handleStatelessExecute(w http.ResponseWriter, r *http.Request) {
	wasmBin, err := readBody(r, maxWasmBytes)
	if err != nil {
		okText(w, http.StatusBadRequest, fmt.Sprintf("WASM error: %v", err))
		return
	}
	text, err := s.sb.run(r.Context(), wasmBin, nil)
	if err != nil {
		okText(w, http.StatusBadRequest, fmt.Sprintf("WASM error: %v", err))
		return
	}
	okText(w, http.StatusOK, text)
}

// handleRegister はデータ登録（POST /data）。ボディ＝データ本体
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(r, maxDataBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read data: %v", err))
		return
	}
	if len(data) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty data")
		return
	}
	rec, err := s.lm.register(data, ownerToken(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"data_id":       rec.DataID,
		"registered_at": rec.CreatedAt,
	})
}

// handleExecute は登録済みデータに対する WASM 実行（POST /data/{id}/execute）。
// ボディ＝WASMバイナリ。応答＝実行結果（stdout、stderrがあれば併記）
func (s *server) handleExecute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wasmBin, err := readBody(r, maxWasmBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read wasm: %v", err))
		return
	}
	if len(wasmBin) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty wasm binary")
		return
	}

	input, err := s.lm.beginExecute(id, ownerToken(r))
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errDeleted):
			// DELETED への execute は 404（§5 不変条件3）
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errBusy):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errForbidden):
			writeJSONError(w, http.StatusForbidden, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	defer s.lm.endExecute(id)

	out, err := s.sb.run(r.Context(), wasmBin, input)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("WASM error: %v", err))
		return
	}
	okText(w, http.StatusOK, out)
}

// handleDelete はデータ削除（DELETE /data/{id}）。応答＝削除証明（JSON）
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cert, err := s.lm.delete(id, ownerToken(r))
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
			writeJSONError(w, http.StatusForbidden, err.Error())
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

// ownerToken は X-Owner-Token ヘッダ（または Authorization: Bearer）からトークンを取り出す。
// トークンの発行元・認可フローの設計は本仕様の対象外（§3, §11）
func ownerToken(r *http.Request) string {
	if t := r.Header.Get("X-Owner-Token"); t != "" {
		return t
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
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
