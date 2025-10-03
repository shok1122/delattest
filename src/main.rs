// Copyright (C) 2023 Gramine contributors
// SPDX-License-Identifier: BSD-3-Clause

// Standard library imports
use std::convert::Infallible; // Used for functions that cannot fail
use std::net::SocketAddr;     // Used to represent an IP address and port
// HTTP library for Rust
use hyper::{Body, Request, Response, Server, Method, StatusCode};
use hyper::service::{make_service_fn, service_fn};

// Embed the contents of "usage.txt" directly into the binary at compile time.
// This avoids reading files at runtime and makes deployment simpler.
const USAGE_TEXT: &str = include_str!("./usage.txt");

// Asynchronous request handler.
// Receives an HTTP request and produces an HTTP response.
// The `Infallible` error type means this function *always* return `Ok(...)`
// (any error is translated into a valid HTTP response).
async fn handle_request(req: Request<Body>) -> Result<Response<Body>, Infallible> {
    match (req.method(), req.uri().path()) {
        // === Handle POST /log requests ===
        // The client sends a text payload that is printed to stdout.
        (&Method::POST, "/log") => {
            // Attempt to read the request body into bytes.
            let body_bytes = match hyper::body::to_bytes(req.into_body()).await {
                Ok(bytes) => bytes,
                Err(e) => {
                    // If the body cannot be read, log the error and return HTTP 400.
                    eprintln!("Error reading request body: {}", e);
                    return Ok(Response::builder()
                        .status(StatusCode::BAD_REQUEST)
                        .body("Failed to read request body".into())
                        .unwrap());
                }
            };

            // Attempt to interpret the bytes as UTF-8 text.
            let body_str = match std::str::from_utf8(&body_bytes) {
                Ok(s) => s,
                Err(e) => {
                    // If decoding fails, log the error and return HTTP 400.
                    eprintln!("Error parsing UTF-8: {}", e);
                    return Ok(Response::builder()
                        .status(StatusCode::BAD_REQUEST)
                        .body("Invalid UTF-8 in request body".into())
                        .unwrap());
                }
            };

            // Print the received message to the server's stdout (server-side log).
            println!("[LOG] Received message: {}", body_str);
            
            // Respond to the client with a success message.
            Ok(Response::new("Message logged successfully".into()))
        }
        
        // === Handle Get / requests ===
        // Provide a usage/help message to the client
        (&Method::GET, "/") => {
            // Use the embedded text file as the response.
            Ok(Response::new(USAGE_TEXT.into()))
        }
        
        // === Handle all other requests ===
        // Any unsupported path or method returns a 404 Not Found.
        _ => {
            Ok(Response::builder()
                .status(StatusCode::NOT_FOUND)
                .body("Not Found".into())
                .unwrap())
        }
    }
}

// By default, tokio spawns as many threads as there are CPU cores. This is undesirable,
// because you need to specify in the Gramine manifest the maximal number of threads per
// process, and ideally this wouldn't depend on your hardware.
//
// See sgx.max_threads in the manifest.
#[tokio::main(worker_threads = 4)]
async fn main() {
    let addr = SocketAddr::from(([127, 0, 0, 1], 3000));
    
    let make_service = make_service_fn(|_conn| async {
        Ok::<_, Infallible>(service_fn(handle_request))
    });
    
    let server = Server::bind(&addr).serve(make_service);
    
    println!("Server running on http://{}", addr);
    println!("Send POST requests to /log with text data");
    
    if let Err(e) = server.await {
        eprintln!("server error: {}", e);
        std::process::exit(1);
    }
}

