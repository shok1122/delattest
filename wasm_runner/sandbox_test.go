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
	out, err := testSandbox().run(context.Background(), noopWasm(), nil, nil)
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
	_, err := sb.run(context.Background(), loopWasm(), nil, nil)
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
	_, err := testSandbox().run(context.Background(), bigMemWasm(), nil, nil)
	if err == nil {
		t.Fatalf("module exceeding memory limit should fail")
	}
}

func TestSandboxRejectsInvalidBinary(t *testing.T) {
	if _, err := testSandbox().run(context.Background(), []byte("not wasm"), nil, nil); err == nil {
		t.Fatalf("invalid binary should fail")
	}
}

// TestSandboxInputMount は登録データが /data/input0 として WASM から読めることを確認する（§8-2）。
// testdata/readinput.wasm は wasm_module/readinput/ を wasi-sdk でビルドしたもの
func TestSandboxInputMount(t *testing.T) {
	wasmBin, err := os.ReadFile("testdata/readinput.wasm")
	if err != nil {
		t.Skipf("testdata/readinput.wasm not found (build it with wasm_module/readinput): %v", err)
	}
	input := []byte("secret-input-42\n")
	out, err := testSandbox().run(context.Background(), wasmBin, [][]byte{input}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != string(input) {
		t.Fatalf("out = %q, want %q", out, input)
	}
}

// TestSandboxMultiInputMount は複数データが指定順に /data/input0, /data/input1, ...
// として見えることを確認する（readinput は input0 から順に連結して出力する）
func TestSandboxMultiInputMount(t *testing.T) {
	wasmBin, err := os.ReadFile("testdata/readinput.wasm")
	if err != nil {
		t.Skipf("testdata/readinput.wasm not found (build it with wasm_module/readinput): %v", err)
	}
	inputs := [][]byte{[]byte("first\n"), []byte("second\n"), []byte("third\n")}
	out, err := testSandbox().run(context.Background(), wasmBin, inputs, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := "first\nsecond\nthird\n"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

// TestSandboxArgs は ?arg= 由来の引数が WASI argv として argv[0]="app.wasm" に
// 続けてモジュールに渡ることを確認する（argsEchoWasm は argv バッファ＝NUL 区切りの
// 全引数をそのまま stdout に書く）
func TestSandboxArgs(t *testing.T) {
	out, err := testSandbox().run(context.Background(), argsEchoWasm(), nil, []string{"get", "github"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := "app.wasm\x00get\x00github\x00"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

// TestSandboxNoArgs は args 指定なしでは argv が空（argv[0] すら無い）ことを確認する（§8-5）
func TestSandboxNoArgs(t *testing.T) {
	out, err := testSandbox().run(context.Background(), argsEchoWasm(), nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "" {
		t.Fatalf("out = %q, want empty (no argv should be provided)", out)
	}
}

// TestSandboxNoInputNoFS は input 無し（ステートレス実行）では /data/input0 が見えないことを確認する
func TestSandboxNoInputNoFS(t *testing.T) {
	wasmBin, err := os.ReadFile("testdata/readinput.wasm")
	if err != nil {
		t.Skipf("testdata/readinput.wasm not found: %v", err)
	}
	out, err := testSandbox().run(context.Background(), wasmBin, nil, nil)
	// readinput は open 失敗時に exit code 1 で終わるため、エラーまたは stderr 出力になる
	if err == nil && !strings.Contains(out, "-- stderr --") {
		t.Fatalf("reading /data/input0 without mount should fail, got out=%q", out)
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
