use anyhow::Result;
use bytes::Bytes;
use hyper::{Method, Request, Response, StatusCode};
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper_util::rt::TokioExecutor;
use hyper_util::server::conn::auto::Builder as AutoBuilder;
use http_body_util::{Full, BodyExt};
use std::{convert::Infallible, net::SocketAddr};
use tokio::net::TcpListener;
use tokio::select;

// Wasmtime (Core Module用 - WASI Preview 1)
use wasmtime::{Config, Engine, Module, Linker, Store};
use wasmtime_wasi::WasiCtxBuilder;
use wasmtime_wasi::preview1::{WasiP1Ctx, add_to_linker_async};
use wasmtime_wasi::p2::pipe::MemoryOutputPipe;

#[tokio::main(flavor = "multi_thread")]
async fn main() -> anyhow::Result<()> {
    let host = std::env::var("HOST").unwrap_or_else(|_| "0.0.0.0".to_string());
    let port: u16 = std::env::var("PORT").ok().and_then(|s| s.parse().ok()).unwrap_or(3000);
    let addr: SocketAddr = format!("{}:{}", host, port).parse().expect("invalid HOST/PORT");

    let listener = TcpListener::bind(addr).await?;
    println!("listening on http://{}", addr);

    use hyper_util::rt::TokioIo;
    select! {
        res = async {
            loop {
                let (io, _peer) = listener.accept().await?;
                tokio::spawn(async move {
                    let svc = service_fn(router);
                    let io = TokioIo::new(io);
                    let builder = AutoBuilder::new(TokioExecutor::new());
                    let conn = builder.serve_connection(io, svc);
                    if let Err(e) = conn.await {
                        eprintln!("server error: {e}");
                    }
                });
            }
            #[allow(unreachable_code)]
            Ok::<(), anyhow::Error>(())
        } => {
            res?;
        },
        _ = tokio::signal::ctrl_c() => {
            eprintln!("Ctrl+C received. stopping the server");
        }
    }
    Ok(())
}

async fn router(req: Request<Incoming>) -> Result<Response<Full<Bytes>>, Infallible> {
    let method = req.method().clone();
    let path = req.uri().path().to_string();

    let resp = match (method, path.as_str()) {
        (Method::GET, "/") => {
            ok_text("OK: POST /execute-wasm (body = WASI Core Module)")
        }
        (Method::POST, "/execute-wasm") => {
            match handle_execute_wasm(req).await {
                Ok(text) => ok_text(text),
                Err(e)   => err_text(StatusCode::BAD_REQUEST, format!("WASM error: {e}")),
            }
        }
        _ => err_text(StatusCode::NOT_FOUND, "not found"),
    };

    Ok(resp)
}

fn ok_text<S: Into<String>>(s: S) -> Response<Full<Bytes>> {
    Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "text/plain; charset=utf-8")
        .body(Full::from(Bytes::from(s.into())))
        .unwrap()
}

fn err_text<S: Into<String>>(code: StatusCode, s: S) -> Response<Full<Bytes>> {
    Response::builder()
        .status(code)
        .header("content-type", "text/plain; charset=utf-8")
        .body(Full::from(Bytes::from(s.into())))
        .unwrap()
}

async fn handle_execute_wasm(req: Request<Incoming>) -> Result<String> {
    let bytes = req.into_body().collect().await?.to_bytes();

    let mut cfg = Config::new();
    cfg.async_support(true);

    // ★ 重要: 巨大な仮想領域予約を止める
    // 予約サイズを小さく（例: 1 MiB）。初期サイズがこれより大きいとこの値は無視されます
    cfg.memory_reservation(1 * 1024 * 1024);
    // 成長用の追加予約も小さく（例: 16 MiB）
    cfg.memory_reservation_for_growth(16 * 1024 * 1024);
    // ガードページを使わない（予約をさらに節約）
    cfg.memory_guard_size(0);
    cfg.guard_before_linear_memory(false);
    // 必要に応じて：成長時にメモリ移動を許可（予約が尽きたら移動）
    cfg.memory_may_move(true);
    // 64-bit メモリは無効のまま（既定で false）
    cfg.wasm_memory64(false);

    let engine = Engine::new(&cfg)?;
    let module = Module::from_binary(&engine, &bytes)?;

    let mut linker = Linker::new(&engine);
    add_to_linker_async(&mut linker, |t: &mut WasiP1Ctx| t)?;

    // ★ 容量は十分に（例: 1MB）。0 は “無制限” ではありません
    let stdout_pipe = MemoryOutputPipe::new(1 * 1024 * 1024);
    let stderr_pipe = MemoryOutputPipe::new(1 * 1024 * 1024);

    // ★ 読み取り用ハンドルをここで clone して保持（同一バッファを共有）
    let stdout_reader = stdout_pipe.clone();
    let stderr_reader = stderr_pipe.clone();

    let wasi = WasiCtxBuilder::new()
        .stdout(stdout_pipe)  // ← 本体を move
        .stderr(stderr_pipe)  // ← 本体を move
        .build_p1();

    let mut store = Store::new(&engine, wasi);
    let instance = linker.instantiate_async(&mut store, &module).await?;
    let start = instance.get_typed_func::<(), ()>(&mut store, "_start")?;
    start.call_async(&mut store, ()).await?;

    // ★ 実行完了後に “reader” 側から中身を読む
    let out = String::from_utf8_lossy(&stdout_reader.contents()).to_string();
    let err = String::from_utf8_lossy(&stderr_reader.contents()).to_string();

    Ok(if err.is_empty() { out } else { format!("-- stdout --\n{out}\n\n-- stderr --\n{err}") })
}

