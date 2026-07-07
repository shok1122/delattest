# =========================
# Builder
# =========================
FROM golang:1.25-bookworm AS builder
ARG BIN_NAME=wasm-runner  # go.mod の module 名と一致させる必要はないが慣例として揃える

WORKDIR /build

# 依存キャッシュを効かせる：go.mod / go.sum のみコピー
COPY go.mod go.sum ./
RUN go mod download

# ソースコードをコピーして一気にビルド（依存キャッシュ有効）
COPY . .

# CGO無効の静的バイナリとしてビルド
RUN CGO_ENABLED=0 go build -o /build/bin/${BIN_NAME} .

# =========================
# Runtime
# =========================
FROM alpine:3.20 AS runtime
ARG BIN_NAME=wasm-runner
COPY --from=builder /build/bin/${BIN_NAME} /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
EXPOSE 3000
