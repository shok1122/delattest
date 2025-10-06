# ========= Base =========
FROM debian:bookworm-slim

# 必要ツール
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl xz-utils make git \
  && rm -rf /var/lib/apt/lists/*

# ======== Install WASI SDK ========
# ここで使うバージョンは必要に応じて変更可（例：24）
ARG WASI_SDK_VER=24
ARG WASI_SDK_REL=${WASI_SDK_VER}.0
#ARG WASI_SDK_TAR=wasi-sdk-${WASI_SDK_REL}-linux.tar.xz
#ARG WASI_SDK_URL=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-${WASI_SDK_VER}/${WASI_SDK_TAR}
ARG WASI_SDK_URL=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-24/wasi-sdk-${WASI_SDK_REL}-x86_64-linux.tar.gz

RUN curl -fsSL ${WASI_SDK_URL} -o /tmp/wasi-sdk.tar.gz \
  && mkdir -p /opt \
  && tar -zxvf /tmp/wasi-sdk.tar.gz -C /opt \
  && ln -s /opt/wasi-sdk-${WASI_SDK_REL}-x86_64-linux /opt/wasi-sdk \
  && rm /tmp/wasi-sdk.tar.gz

# PATH / 環境変数
ENV WASI_SDK_PATH=/opt/wasi-sdk
ENV PATH="${WASI_SDK_PATH}/bin:${PATH}"

# 作業ディレクトリ
WORKDIR /work

# （任意）プロジェクトをコンテナにコピーする場合
# COPY . /work

# 使い方例:
# docker build -t wasi-build .
# docker run --rm -v "$PWD":/work wasi-build \
#   clang --sysroot=$WASI_SDK_PATH/share/wasi-sysroot \
#   --target=wasm32-wasi -O2 -o hello.wasm hello.c

