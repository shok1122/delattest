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

// Wasmtime (Component Model / WASI Preview2)
use wasmtime::{Config, Engine, component::{Component, Linker}};
use wasmtime::Store;
use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiView};
use wasmtime_wasi::p2::bindings::Command;
use wasmtime::component::ResourceTable;

#[derive(Default)]
struct Ctx {
    wasi: WasiCtx,
    table: ResourceTable,
}
impl WasiView for Ctx {
    fn ctx(&mut self) -> wasmtime_wasi::WasiCtxView<'_> {
        wasmtime_wasi::WasiCtxView {
            ctx: &mut self.wasi,
            table: &mut self.table,
        }
    }
}

#[tokio::main(flavor = "multi_thread")]
async fn main() -> anyhow::Result<()> {
    let host = std::env::var("HOST").unwrap_or_else(|_| "0.0.0.0".to_string());
    let port: u16 = std::env::var("PORT").ok().and_then(|s| s.parse().ok()).unwrap_or(3000);
    let addr: SocketAddr = format!("{}:{}", host, port).parse().expect("invalid HOST/PORT");

    let listener = TcpListener::bind(addr).await?;
    println!("listening on http://{}", addr);

    use hyper_util::rt::TokioIo;
    select! {
        _ = async {
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
            Ok::<(), anyhow::Error>(())
        } =>{},
        _ = tokio::signal::ctrl_c() => {
            eprintln!("Ctrl+C received. stopping the servet");
        }
    }
    Ok(())
}

async fn router(req: Request<Incoming>) -> Result<Response<Full<Bytes>>, Infallible> {
    let method = req.method().clone();
    let path = req.uri().path().to_string();

    let resp = match (method, path.as_str()) {
        (Method::GET, "/") => {
            ok_text("OK: POST /execute-wasm (body = WASI Preview2 component)")
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
    // リクエストボディを全部読み込み（Hyper 1.x では BodyExt::collect → to_bytes）
    let bytes = req.into_body().collect().await?.to_bytes();

    // Wasmtime エンジン（Component Model + async）
    let mut cfg = Config::new();
    cfg.wasm_component_model(true).async_support(true);
    let engine = Engine::new(&cfg)?;

    // 受け取った .wasm は「WASI Preview2 component」を想定
    let component = Component::from_binary(&engine, &bytes)?;

    // Linker に WASI P2 を追加（async 版）
    let mut linker: Linker<Ctx> = Linker::new(&engine);
    wasmtime_wasi::p2::add_to_linker_async(&mut linker)?; // sync 版もあり。用途で選択。 [oai_citation:5‡Wasmtime](https://docs.wasmtime.dev/api/wasmtime_wasi/p2/fn.add_to_linker_sync.html?utm_source=chatgpt.com)

    // 実行コンテキスト（stdio/args などは適宜調整）
    let wasi = WasiCtxBuilder::new()
        .inherit_stdio()
        .inherit_args()
        .build();

    let mut store = Store::new(&engine, Ctx { 
        wasi,
        table: ResourceTable::new(),
    });

    // `wasi:cli/command` の run() を呼ぶ
    let cmd = Command::instantiate_async(&mut store, &component, &linker).await?;
    let result = cmd.wasi_cli_run().call_run(&mut store).await?;

    match result {
        Ok(()) => Ok("component finished successfully".to_string()),
        Err(()) => Ok("component finished with error".to_string()),
    }
}
