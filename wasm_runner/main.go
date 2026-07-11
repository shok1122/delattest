package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	// WASMランナーサーバで使うアドレスの決定
	host := getEnv("HOST", "0.0.0.0")
	port := getEnv("PORT", "3000")
	addr := net.JoinHostPort(host, port)

	// 封印ストレージの初期化（§6.2）
	// Gramine実行時は DATA_DIR が encrypted mount（Protected Files）を指すため、
	// ホスト上のファイルはすべて暗号化された状態でしか存在しない
	dataDir := getEnv("DATA_DIR", "data_store")
	st, err := newStore(dataDir)
	if err != nil {
		log.Fatalf("storage init error: %v", err)
	}

	// オーナー鍵管理の初期化（TOFU）。登録済みならロードし、未登録なら
	// POST /owner での初回登録を待つ（登録までコマンドはすべて拒否される）
	om, err := newOwnerManager(st)
	if err != nil {
		log.Fatalf("owner manager init error: %v", err)
	}
	if k, ok := om.key(); ok {
		log.Printf("owner key registered: %s", k)
	} else {
		log.Printf("owner key not registered yet: register via POST /owner (commands are rejected until then)")
	}

	// 署名認証（Ed25519）。リプレイ対策のタイムスタンプ許容ウィンドウは
	// AUTH_WINDOW_SEC（秒）で調整できる
	windowSec := envInt("AUTH_WINDOW_SEC", 300)
	if windowSec < 1 {
		log.Fatalf("AUTH_WINDOW_SEC must be positive: %d", windowSec)
	}
	auth := newAuthenticator(time.Duration(windowSec) * time.Second)

	// プログラムレジストリの初期化（§3.2）。永続化済みメタデータを読み込む
	preg, err := newProgramRegistry(st)
	if err != nil {
		log.Fatalf("program registry init error: %v", err)
	}

	// 削除証明の発行モジュール（§6.4）。起動時に署名鍵を生成し、
	// 公開鍵ハッシュを埋め込んだ quote を一度だけ取得する（§9.3）
	pr := newProver()

	// ライフサイクル管理の初期化（§6.1）。永続化済みメタデータの読み込みと
	// 中断された削除処理のリカバリを行う
	lm, err := newLifecycleManager(st, pr)
	if err != nil {
		log.Fatalf("lifecycle init error: %v", err)
	}

	// WASM実行サンドボックスの制約（§8-4）
	memPages := envInt("WASM_MEM_LIMIT_PAGES", 1024) // 64 KiB/page × 1024 = 64 MiB
	if memPages < 1 || memPages > 65536 {
		log.Fatalf("WASM_MEM_LIMIT_PAGES out of range (1-65536): %d", memPages)
	}
	sb := &sandbox{
		execTimeout:   time.Duration(envInt("EXEC_TIMEOUT_SEC", 30)) * time.Second,
		memLimitPages: uint32(memPages),
	}

	// サーバを goroutine で起動
	srv := &http.Server{Addr: addr, Handler: newHandler(lm, sb, preg, om, auth)}
	go func() {
		log.Printf("listening on http://%s (attestation: %s, data dir: %s)", addr, pr.attestationType, dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	// シグナル待ち受け
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Println("Ctrl+C received. stopping the server")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("invalid %s: %v", key, err)
	}
	return n
}
