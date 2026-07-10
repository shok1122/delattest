package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing/fstest"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// outputCap bounds how much stdout/stderr we buffer from a running module,
// mirroring the fixed-capacity pipes used by the previous Rust implementation.
// これはリソース枯渇（DoS）防止のための制限であり、機密性の担保が目的ではない（§8-3）
const outputCap = 1 * 1024 * 1024

// sandbox は WASM 実行サンドボックス（§6.3, §8）。
//   - ネットワーク系ホスト関数は一切 instantiate しない。wasi_snapshot_preview1 の
//     Core Module のみを提供し、WASI Socket 拡張等を将来も追加しないことが設計原則（§8-1）
//   - ファイルシステムは inputs がある場合のみ /data/input0, /data/input1, ... を
//     読み取り専用で見せる。書き込み用マウントは提供しない（§8-2）
//   - 時計・乱数は wazero デフォルトの決定的な擬似値のまま。実時間・実乱数といった
//     ホスト資源は与えない（§8-5 最小権限）
type sandbox struct {
	execTimeout   time.Duration
	memLimitPages uint32
}

// run は WASM バイナリを制約付きで実行し、stdout（stderr があれば併記）を返す。
// inputs が非空の場合、i 番目の内容を読み取り専用ファイル /data/input<i> として
// WASM 側に見せる。メモリ上の FS としてマウントするため平文がディスクに触れることはない。
// args が非空の場合、WASI argv として argv[0]="app.wasm" に続けて渡す。
// argv はリクエスト元が明示指定した実行パラメータであり、時計・乱数のような
// ホスト資源ではないため、§8-5（最小権限）には抵触しない
func (s *sandbox) run(ctx context.Context, wasmBin []byte, inputs [][]byte, args []string) (string, error) {
	// 実行タイムアウト（§8-4）。CloseOnContextDone により、無限ループ等で
	// 実行中のコードも期限超過時に強制中断される
	ctx, cancel := context.WithTimeout(ctx, s.execTimeout)
	defer cancel()

	rcfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(s.memLimitPages) // メモリ上限（§8-4）
	runtime := wazero.NewRuntimeWithConfig(ctx, rcfg)
	defer runtime.Close(context.Background())

	// WASI（WebAssembly System Interface）のセットアップ。
	// Core Module 相当のみで、ソケット系ホスト関数は含まれない
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
		return "", err
	}

	// WASMバイナリをWASMモジュールとしてコンパイル
	compiled, err := runtime.CompileModule(ctx, wasmBin)
	if err != nil {
		return "", err
	}

	// 出力バッファの用意
	stdout := newCappedBuffer(outputCap)
	stderr := newCappedBuffer(outputCap)
	config := wazero.NewModuleConfig().
		WithStdout(stdout).
		WithStderr(stderr)
	if len(args) > 0 {
		config = config.WithArgs(append([]string{"app.wasm"}, args...)...)
	}
	if len(inputs) > 0 {
		fsys := fstest.MapFS{}
		for i, input := range inputs {
			fsys[fmt.Sprintf("input%d", i)] = &fstest.MapFile{Data: input, Mode: 0o444}
		}
		config = config.WithFSConfig(wazero.NewFSConfig().WithFSMount(fsys, "/data"))
	}

	// モジュールの実行
	mod, err := runtime.InstantiateModule(ctx, compiled, config)
	if mod != nil {
		defer mod.Close(context.Background())
	}

	// 終了処理
	if err != nil {
		var exitErr *sys.ExitError
		switch {
		// WASIプログラムが正常終了する際，wazero上ではエラーとして返ってくるので，それを判定
		case errors.As(err, &exitErr) && exitErr.ExitCode() == 0:
			// _start called proc_exit(0); not an error.
		case ctx.Err() != nil:
			return "", fmt.Errorf("execution aborted: %v (limit %s)", ctx.Err(), s.execTimeout)
		default:
			return "", err
		}
	}

	// 結果の整形
	out := stdout.String()
	errOut := stderr.String()
	if errOut == "" {
		return out, nil
	}
	return fmt.Sprintf("-- stdout --\n%s\n\n-- stderr --\n%s", out, errOut), nil
}

// cappedBuffer stops accepting bytes once it reaches its capacity instead of
// growing without bound, so a runaway module can't exhaust host memory.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}
