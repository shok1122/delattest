FROM ubuntu:22.04

RUN apt-get update && apt-get install -y \
    wget \
    tar \
    && rm -rf /var/lib/apt/lists/*

# WASI SDK のダウンロードとインストール
RUN wget https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-20/wasi-sdk-20.0-linux.tar.gz \
    && tar xzf wasi-sdk-20.0-linux.tar.gz \
    && mv wasi-sdk-20.0 /opt/wasi-sdk \
    && rm wasi-sdk-20.0-linux.tar.gz

ENV PATH="/opt/wasi-sdk/bin:${PATH}"

WORKDIR /workspace

