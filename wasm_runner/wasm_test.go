package main

// テスト用の手組み最小WASMモジュール群。
// 外部ツールチェーン無しで実行制約（タイムアウト・メモリ上限）を検証するために、
// WASMバイナリフォーマットを直接組み立てている

func wasmHeader() []byte {
	return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00} // "\0asm" + version 1
}

// noopWasm は何もせず正常終了する _start だけを持つモジュール
func noopWasm() []byte {
	return append(wasmHeader(),
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type section: () -> ()
		0x03, 0x02, 0x01, 0x00, // function section: func[0] = type 0
		0x07, 0x0a, 0x01, 0x06, '_', 's', 't', 'a', 'r', 't', 0x00, 0x00, // export "_start"
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b, // code: {}
	)
}

// loopWasm は _start が無限ループするモジュール（タイムアウト検証用）
func loopWasm() []byte {
	return append(wasmHeader(),
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0a, 0x01, 0x06, '_', 's', 't', 'a', 'r', 't', 0x00, 0x00,
		0x0a, 0x09, 0x01, 0x07, 0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b, // code: loop { br 0 }
	)
}

// bigMemWasm は min 2000ページ（125MiB）のメモリを要求するモジュール（メモリ上限検証用）
func bigMemWasm() []byte {
	return append(wasmHeader(),
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x05, 0x04, 0x01, 0x00, 0xd0, 0x0f, // memory section: min = 2000 pages (LEB128)
		0x07, 0x0a, 0x01, 0x06, '_', 's', 't', 'a', 'r', 't', 0x00, 0x00,
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	)
}

// argsEchoWasm は WASI argv バッファ（全引数を NUL 区切りで連結したもの）を
// そのまま stdout に書くモジュール（argv 受け渡し検証用）。
// args_sizes_get(argc→addr0, buf_size→addr4) → args_get(argv→addr8, buf→addr1024)
// → fd_write(fd=1, iovec@addr512={base:1024, len:*addr4}) の順に呼ぶ
func argsEchoWasm() []byte {
	name := func(s string) []byte { return append([]byte{byte(len(s))}, s...) }
	b := wasmHeader()
	// type section: type0 (i32,i32)->i32, type1 (i32,i32,i32,i32)->i32, type2 ()->()
	b = append(b,
		0x01, 0x12, 0x03,
		0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7f,
		0x60, 0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7f,
		0x60, 0x00, 0x00,
	)
	// import section: func[0]=args_sizes_get(type0), func[1]=args_get(type0), func[2]=fd_write(type1)
	imports := []byte{0x03}
	for _, imp := range []struct {
		fn      string
		typeIdx byte
	}{{"args_sizes_get", 0x00}, {"args_get", 0x00}, {"fd_write", 0x01}} {
		imports = append(imports, name("wasi_snapshot_preview1")...)
		imports = append(imports, name(imp.fn)...)
		imports = append(imports, 0x00, imp.typeIdx) // import kind: func
	}
	b = append(b, 0x02, byte(len(imports)))
	b = append(b, imports...)
	// function section: func[3] = type 2
	b = append(b, 0x03, 0x02, 0x01, 0x02)
	// memory section: 1 memory, min 1 page
	b = append(b, 0x05, 0x03, 0x01, 0x00, 0x01)
	// export section: "memory" mem 0, "_start" func 3
	exports := []byte{0x02}
	exports = append(exports, name("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, name("_start")...)
	exports = append(exports, 0x00, 0x03)
	b = append(b, 0x07, byte(len(exports)))
	b = append(b, exports...)
	// code section
	body := []byte{
		0x00,                         // no locals
		0x41, 0x00,                   // i32.const 0   (argc の書き込み先)
		0x41, 0x04,                   // i32.const 4   (buf_size の書き込み先)
		0x10, 0x00, 0x1a,             // call args_sizes_get; drop
		0x41, 0x08,                   // i32.const 8    (argv ポインタ配列)
		0x41, 0x80, 0x08,             // i32.const 1024 (argv バッファ)
		0x10, 0x01, 0x1a,             // call args_get; drop
		0x41, 0x80, 0x04,             // i32.const 512
		0x41, 0x80, 0x08,             // i32.const 1024
		0x36, 0x02, 0x00,             // i32.store      (iovec.base = 1024)
		0x41, 0x84, 0x04,             // i32.const 516
		0x41, 0x04,                   // i32.const 4
		0x28, 0x02, 0x00,             // i32.load       (buf_size)
		0x36, 0x02, 0x00,             // i32.store      (iovec.len = buf_size)
		0x41, 0x01,                   // i32.const 1    (fd = stdout)
		0x41, 0x80, 0x04,             // i32.const 512  (iovec)
		0x41, 0x01,                   // i32.const 1    (iovec 数)
		0x41, 0x88, 0x04,             // i32.const 520  (nwritten の書き込み先)
		0x10, 0x02, 0x1a,             // call fd_write; drop
		0x0b,                         // end
	}
	b = append(b, 0x0a, byte(len(body)+2), 0x01, byte(len(body)))
	return append(b, body...)
}
