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

    // Wasmtime エンジン（Core Module用、async対応）
    let mut cfg = Config::new();
    cfg.async_support(true);
    let engine = Engine::new(&cfg)?;

    // Core WASM Module として読み込む
    let module = Module::from_binary(&engine, &bytes)?;

    // Linker に WASI Preview1 を追加
    let mut linker = Linker::new(&engine);
    add_to_linker_async(&mut linker, |t: &mut WasiP1Ctx| t)?;

    // WASIコンテキスト作成（Preview 1用）
    let wasi = WasiCtxBuilder::new()
        .inherit_stdio()
        .inherit_args()
        .build_p1();

    let mut store = Store::new(&engine, wasi);

    // インスタンス化
    let instance = linker.instantiate_async(&mut store, &module).await?;
    
    // _start 関数を呼び出し（WASIのエントリポイント）
    let start = instance.get_typed_func::<(), ()>(&mut store, "_start")?;
    start.call_async(&mut store, ()).await?;

    Ok("WASM module executed successfully".to_string())
}
