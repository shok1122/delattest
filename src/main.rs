// Copyright (C) 2023 Gramine contributors
// SPDX-License-Identifier: BSD-3-Clause
use std::convert::Infallible;
use std::net::SocketAddr;
use hyper::{Body, Request, Response, Server, Method, StatusCode};
use hyper::service::{make_service_fn, service_fn};
use wasmtime::*;

const USAGE_TEXT: &str = r#"HTTP Logger Server with WASM Execution

Usage:
  POST /log          - Log text message to server console
  POST /execute-wasm - Execute WASM binary
  GET  /             - Show this usage information

Examples:
  # Log a message
  curl -X POST http://localhost:3000/log -d "Hello from client!"
  
  # Execute WASM binary
  curl -X POST http://localhost:3000/execute-wasm \
       --data-binary @hello.wasm \
       -H "Content-Type: application/wasm"

WASM Compilation Examples:
  # C: clang --target=wasm32-wasi -o hello.wasm hello.c
  # Rust: cargo build --target wasm32-wasi --release
  # Go: GOOS=wasip1 GOARCH=wasm go build -o hello.wasm main.go
"#;

async fn handle_request(req: Request<Body>) -> Result<Response<Body>, Infallible> {
    match (req.method(), req.uri().path()) {
        // POST /log エンドポイントで文字列を受け取る
        (&Method::POST, "/log") => {
            let body_bytes = match hyper::body::to_bytes(req.into_body()).await {
                Ok(bytes) => bytes,
                Err(e) => {
                    eprintln!("Error reading request body: {}", e);
                    return Ok(Response::builder()
                        .status(StatusCode::BAD_REQUEST)
                        .body("Failed to read request body".into())
                        .unwrap());
                }
            };

            let body_str = match std::str::from_utf8(&body_bytes) {
                Ok(s) => s,
                Err(e) => {
                    eprintln!("Error parsing UTF-8: {}", e);
                    return Ok(Response::builder()
                        .status(StatusCode::BAD_REQUEST)
                        .body("Invalid UTF-8 in request body".into())
                        .unwrap());
                }
            };

            // サーバー側のログに出力
            println!("[LOG] Received message: {}", body_str);
            
            Ok(Response::new("Message logged successfully".into()))
        }
        
        // POST /execute-wasm エンドポイントでWASMバイナリを受け取って実行
        (&Method::POST, "/execute-wasm") => {
            let body_bytes = match hyper::body::to_bytes(req.into_body()).await {
                Ok(bytes) => bytes,
                Err(e) => {
                    eprintln!("Error reading WASM binary: {}", e);
                    return Ok(Response::builder()
                        .status(StatusCode::BAD_REQUEST)
                        .body("Failed to read WASM binary".into())
                        .unwrap());
                }
            };

            println!("[WASM] Received WASM binary ({} bytes)", body_bytes.len());
            
            // WASM実行
            match execute_wasm(&body_bytes).await {
                Ok(output) => {
                    println!("[WASM] Execution successful");
                    Ok(Response::new(format!("WASM executed successfully\n\nResult: {}", output).into()))
                }
                Err(e) => {
                    eprintln!("[WASM] Execution failed: {}", e);
                    Ok(Response::builder()
                        .status(StatusCode::BAD_REQUEST)
                        .body(format!("WASM execution failed: {}", e).into())
                        .unwrap())
                }
            }
        }
        
        // GET / エンドポイントで使用方法を説明
        (&Method::GET, "/") => {
            Ok(Response::new(USAGE_TEXT.into()))
        }
        
        // その他のリクエストに対しては404を返す
        _ => {
            Ok(Response::builder()
                .status(StatusCode::NOT_FOUND)
                .body("Not Found".into())
                .unwrap())
        }
    }
}

async fn execute_wasm(wasm_bytes: &[u8]) -> Result<String, Box<dyn std::error::Error + Send + Sync>> {
    // Wasmtimeエンジンの作成
    let engine = Engine::default();
    let module = Module::new(&engine, wasm_bytes)?;
    
    // WASI設定を含むStoreの作成
    let wasi = wasmtime_wasi::WasiCtxBuilder::new()
        .inherit_stdio()
        .inherit_args()
        .build();
    let mut store = Store::new(&engine, wasi);
    
    // リンカーの作成とWASI関数の追加
    let mut linker = Linker::new(&engine);
    wasmtime_wasi::add_to_linker(&mut linker, |s| s)?;
    
    // WASMモジュールのインスタンス化
    let instance = linker.instantiate(&mut store, &module)?;
    
    // _start関数を実行（WASIの標準エントリポイント）
    if let Ok(start_func) = instance.get_typed_func::<(), ()>(&mut store, "_start") {
        match start_func.call(&mut store, ()) {
            Ok(()) => {
                return Ok("WASM _start function executed successfully".to_string());
            }
            Err(trap) => {
                // トラップでもある程度の情報を返す
                return Ok(format!("WASM executed but ended with trap: {}", trap));
            }
        }
    }
    
    // main関数を実行
    if let Ok(main_func) = instance.get_typed_func::<(), i32>(&mut store, "main") {
        match main_func.call(&mut store, ()) {
            Ok(return_code) => {
                return Ok(format!("WASM main function executed, returned: {}", return_code));
            }
            Err(trap) => {
                return Err(format!("WASM main function trap: {}", trap).into());
            }
        }
    }
    
    Err("No suitable entry function (_start or main) found in WASM module".into())
}

#[tokio::main(worker_threads = 4)]
async fn main() {
    let addr = SocketAddr::from(([127, 0, 0, 1], 3000));
    
    let make_service = make_service_fn(|_conn| async {
        Ok::<_, Infallible>(service_fn(handle_request))
    });
    
    let server = Server::bind(&addr).serve(make_service);
    
    println!("Server running on http://{}", addr);
    println!("WASI-enabled WASM executor ready!");
    
    if let Err(e) = server.await {
        eprintln!("server error: {}", e);
        std::process::exit(1);
    }
}
