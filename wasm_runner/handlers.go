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
	um *userManager
}

// newHandler は API ルーティング（§7）を構築する。
// 生値（平文）を直接返す API は存在しない（§5 不変条件1）:
// register は書き込み専用、execute は WASM プログラムの計算結果のみを返す
func newHandler(lm *lifecycleManager, sb *sandbox, um *userManager) http.Handler {
	s := &server{lm: lm, sb: sb, um: um}
	mux := http.NewServeMux()
	// ヘルスチェック --- GET /
	mux.HandleFunc("GET /{$}", s.handleHealth)
	// ユーザ発行（owner_id + APIキー）
	mux.HandleFunc("POST /users", s.handleCreateUser)
	// WASM 実行（使用する登録済みデータを ?data=<id> の繰り返しで0個以上指定）
	mux.HandleFunc("POST /execute", s.handleExecute)
	// ライフサイクル管理 API
	mux.HandleFunc("POST /data", s.handleRegister)
	mux.HandleFunc("DELETE /data/{id}", s.handleDelete)
	mux.HandleFunc("GET /data/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /data/{id}/proof", s.handleProof)
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	okText(w, http.StatusOK, "OK: POST /users | POST /execute?data=<id>&data=... (wasm binary as body; zero or more data ids) | POST /data | DELETE /data/{id} | GET /data/{id}/status | GET /data/{id}/proof")
}

// handleCreateUser はユーザ発行（POST /users）。owner_id と API キーを新規発行する。
// API キーの平文はこの応答限りで、以後サーバはハッシュしか持たない（§4.1）。
// 発行エンドポイント自体の保護は設計書 §11 の未解決課題
func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	rec, apiKey, err := s.um.createUser()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"owner_id":   rec.OwnerID,
		"api_key":    apiKey,
		"created_at": rec.CreatedAt,
	})
}

// handleRegister はデータ登録（POST /data）。ボディ＝データ本体。認証必須で、
// 認証済みユーザの owner_id がデータの所有者として記録される
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ownerID, err := s.um.resolveOwner(apiKey(r))
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, err.Error())
		return
	}
	data, err := readBody(r, maxDataBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read data: %v", err))
		return
	}
	if len(data) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty data")
		return
	}
	rec, err := s.lm.register(data, ownerID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"data_id":       rec.DataID,
		"registered_at": rec.CreatedAt,
	})
}

// handleExecute は WASM 実行（POST /execute）。ボディ＝WASMバイナリ。
// クエリパラメータ data の繰り返しで登録済みデータを0個以上指定でき、指定順の
// i 番目が読み取り専用ファイル /data/input<i> として WASM から見える。
// データを1個以上指定する場合は認証必須で、全データが認証済みユーザの所有で
// なければならない。data 指定なしはステートレス実行（ライフサイクル管理・
// 削除証明の対象外、認証不要）。応答＝実行結果（stdout、stderrがあれば併記）
func (s *server) handleExecute(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["data"]
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

	var ownerID string
	if len(ids) > 0 {
		var err error
		if ownerID, err = s.um.resolveOwner(apiKey(r)); err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}
	}

	wasmBin, err := readBody(r, maxWasmBytes)
	if err != nil {
		writeJSONError(w, bodyErrStatus(err), fmt.Sprintf("read wasm: %v", err))
		return
	}
	if len(wasmBin) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty wasm binary")
		return
	}

	// 指定された全データを原子的に IN_USE にする（全データの所有者が本人であることを照合）
	inputs, err := s.lm.beginExecute(ids, ownerID)
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
	defer s.lm.endExecute(ids)

	out, err := s.sb.run(r.Context(), wasmBin, inputs)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("WASM error: %v", err))
		return
	}
	okText(w, http.StatusOK, out)
}

// handleDelete はデータ削除（DELETE /data/{id}）。認証必須（所有者本人のみ）。
// 応答＝削除証明（JSON）
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ownerID, err := s.um.resolveOwner(apiKey(r))
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, err.Error())
		return
	}
	id := r.PathValue("id")
	cert, err := s.lm.delete(id, ownerID)
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

// apiKey は X-API-Key ヘッダ（または Authorization: Bearer）から API キーを取り出す。
// キー → owner_id の解決（認証）は userManager.resolveOwner が行う（§4.1）
func apiKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
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
