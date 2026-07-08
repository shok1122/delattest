package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func testSandbox() *sandbox {
	return &sandbox{execTimeout: 10 * time.Second, memLimitPages: 1024}
}

func TestSandboxRunsNoopModule(t *testing.T) {
	out, err := testSandbox().run(context.Background(), noopWasm(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
}

// TestSandboxTimeout は無限ループするモジュールがタイムアウトで強制中断されることを確認する（§8-4）
func TestSandboxTimeout(t *testing.T) {
	sb := &sandbox{execTimeout: 500 * time.Millisecond, memLimitPages: 1024}
	start := time.Now()
	_, err := sb.run(context.Background(), loopWasm(), nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("infinite loop should be aborted")
	}
	if !strings.Contains(err.Error(), "execution aborted") {
		t.Fatalf("err = %v, want execution aborted", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("abort took too long: %s", elapsed)
	}
}

// TestSandboxMemoryLimit はメモリ上限を超える要求を持つモジュールが拒否されることを確認する（§8-4）
func TestSandboxMemoryLimit(t *testing.T) {
	// bigMemWasm は 2000 ページ（125MiB）を要求するが、上限は 1024 ページ（64MiB）
	_, err := testSandbox().run(context.Background(), bigMemWasm(), nil)
	if err == nil {
		t.Fatalf("module exceeding memory limit should fail")
	}
}

func TestSandboxRejectsInvalidBinary(t *testing.T) {
	if _, err := testSandbox().run(context.Background(), []byte("not wasm"), nil); err == nil {
		t.Fatalf("invalid binary should fail")
	}
}

// TestSandboxInputMount は登録データが /data/input として WASM から読めることを確認する（§8-2）。
// testdata/readinput.wasm は wasm_module/readinput/ を wasi-sdk でビルドしたもの
func TestSandboxInputMount(t *testing.T) {
	wasmBin, err := os.ReadFile("testdata/readinput.wasm")
	if err != nil {
		t.Skipf("testdata/readinput.wasm not found (build it with wasm_module/readinput): %v", err)
	}
	input := []byte("secret-input-42\n")
	out, err := testSandbox().run(context.Background(), wasmBin, input)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != string(input) {
		t.Fatalf("out = %q, want %q", out, input)
	}
}

// TestSandboxNoInputNoFS は input 無し（ステートレス実行）では /data/input が見えないことを確認する
func TestSandboxNoInputNoFS(t *testing.T) {
	wasmBin, err := os.ReadFile("testdata/readinput.wasm")
	if err != nil {
		t.Skipf("testdata/readinput.wasm not found: %v", err)
	}
	out, err := testSandbox().run(context.Background(), wasmBin, nil)
	// readinput は open 失敗時に exit code 1 で終わるため、エラーまたは stderr 出力になる
	if err == nil && !strings.Contains(out, "-- stderr --") {
		t.Fatalf("reading /data/input without mount should fail, got out=%q", out)
	}
}

func TestCappedBuffer(t *testing.T) {
	buf := newCappedBuffer(8)
	// 上限をまたぐ書き込みは受理できた分だけを返す
	n, err := buf.Write([]byte("0123456789"))
	if err != nil || n != 8 {
		t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
	}
	if buf.String() != "01234567" {
		t.Fatalf("buf = %q, want capped at 8 bytes", buf.String())
	}
	// 上限到達後の書き込みも「成功」として扱われる（モジュールを止めないため）
	n, err = buf.Write([]byte("abc"))
	if err != nil || n != 3 {
		t.Fatalf("Write after cap = (%d, %v), want (3, nil)", n, err)
	}
	if buf.String() != "01234567" {
		t.Fatalf("buf grew past cap: %q", buf.String())
	}
}
