# =========================
# Builder
# =========================
FROM rust:1.86-slim AS builder
ARG BIN_NAME=wasm-runner  # Cargo.toml の [package].name と一致させる

# musl (静的リンク) 環境を準備
RUN apt-get update \
 && apt-get install -y --no-install-recommends musl-tools ca-certificates pkg-config \
 && rm -rf /var/lib/apt/lists/*
RUN rustup target add x86_64-unknown-linux-musl

WORKDIR /build

# 依存キャッシュを効かせる：Cargo.toml / Cargo.lock のみコピー
COPY Cargo.toml Cargo.lock ./

# ソースコードをコピーして一気にビルド（依存キャッシュ有効）
COPY . .

# 依存と本体をまとめてビルド
RUN cargo build --release --target x86_64-unknown-linux-musl --locked

# =========================
# Runtime
# =========================
FROM alpine:3.20 AS runtime
ARG BIN_NAME=wasm-runner
COPY --from=builder /build/target/x86_64-unknown-linux-musl/release/${BIN_NAME} /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
EXPOSE 3000
