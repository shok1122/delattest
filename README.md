# delattest

WASM モジュールを HTTP 経由で受け取り、Gramine-SGX のエンクレーブ内で実行する
サーバー（`wasm-runner`）。動作確認済み（2026-07-06, Ubuntu 22.04 / Azure VM）。

## 構成

```
wasm_runner/        wasm-runner 本体（Rust, hyper + wasmtime）。Docker(musl)でビルド
wasm_module/        動作確認用の WASM モジュール（hello1, hello2 など, wasi-sdk でビルド）
Makefile            リポジトリ直下。gramine-manifest / gramine-sgx-sign / 実行を行う
wasm-runner.manifest.template   Gramine マニフェストのテンプレート（リポジトリ管理下）
cache/wasm-runner   実行バイナリの配置場所（.gitignore 対象、ビルドして都度生成）
```

`cache/wasm-runner`（実行バイナリ）は `.gitignore` でリポジトリから除外されているため、
**新しいマシンでは毎回ゼロから用意し直す必要がある**。本ドキュメントはそのための手順。

## 前提

- Ubuntu 22.04 (jammy)
- SGX 対応 CPU、`/dev/sgx_enclave` が存在すること
- sudo 権限

## セットアップ手順（新規マシン）

### 1. Gramine のインストール

Ubuntu 標準の apt リポジトリには含まれないため、Gramine 公式リポジトリを追加する。

```sh
sudo curl -fsSLo /usr/share/keyrings/gramine-keyring.gpg \
  https://packages.gramineproject.io/gramine-keyring.gpg

echo 'deb [signed-by=/usr/share/keyrings/gramine-keyring.gpg] https://packages.gramineproject.io/ jammy main' \
  | sudo tee /etc/apt/sources.list.d/gramine.list

sudo apt-get update
sudo apt-get install -y gramine
```

補足: `https://packages.gramineproject.io/gramine.list` という案内をたまに見かけるが
現在は配布されていない（404）。上記のように `dists/jammy` を指す1行を自分で書く。

### 2. SGX 署名鍵の生成（初回のみ）

```sh
gramine-sgx-gen-private-key
```

`~/.config/gramine/enclave-key.pem` に鍵が作られる（テスト用の自己署名鍵）。

### 3. Docker のインストール（wasm-runner のビルドに使用）

```sh
sudo apt-get install -y docker.io
sudo systemctl enable --now docker
sudo usermod -aG docker $(whoami)   # 反映には再ログイン or `sg docker` が必要
```

### 4. wasm-runner バイナリのビルド

`wasm_runner/` は musl 静的リンクでビルドする Rust プロジェクト。Cargo.lock も
`.gitignore` 対象なので無ければ生成する。

```sh
cd wasm_runner

# Cargo.lock が無ければ生成
[ -f Cargo.lock ] || docker run --rm -v .:/work -w /work rust:1.86-slim \
  cargo generate-lockfile

# ビルド用イメージを作成してビルド（数分〜10分程度）
docker build --target builder -t rust-sgx-builder .

# ビルド済みバイナリを取り出す
docker create --name tmp-extract rust-sgx-builder
docker cp tmp-extract:/build/target/x86_64-unknown-linux-musl/release/wasm-runner ./cache/wasm-runner
docker rm tmp-extract
sudo chown $(whoami):$(whoami) ./cache/wasm-runner

# リポジトリ直下の cache/ に配置（root Makefile が参照する場所）
cp ./cache/wasm-runner ../cache/wasm-runner
cd ..
```

### 5. マニフェスト生成・署名・実行

```sh
SGX=1 make          # gramine-manifest → gramine-sgx-sign（wasm-runner.manifest{,.sgx}, .sig を生成）
SGX=1 make run      # gramine-sgx wasm-runner を起動（0.0.0.0:3000 で待受）
```

非 SGX（gramine-direct）で試す場合は `SGX=1` を外すだけでよい。

### 6. 動作確認（別ターミナル / クライアント側）

`wasm_module/` 側で WASM モジュールをビルドしてサーバーに送信する。

```sh
cd wasm_module

# wasi-sdk ビルド環境のイメージを作成（初回のみ）
docker build -t wasi-build .

# hello1 をビルド（main.c → app.wasm）
make -C hello1 all

# サーバーに送信して実行（このマシンで動かしている wasm-runner 宛て: localhost:3000）
make test-local SRC=hello1

# もしくは本番ドメイン (delattest.dev:3000) で稼働しているサーバー宛て
make test SRC=hello1
```

`test` と `test-local` の違いは送信先ホストだけ（`wasm_module/Makefile` 参照）。

| ターゲット | 送信先 |
|---|---|
| `test-local` | `http://localhost:3000` — 手元のマシンで起動した `wasm-runner` を直接叩く |
| `test` | `http://delattest.dev:3000` — 本番ドメインで稼働中のサーバーを叩く |

このセットアップ手順どおりに自分のマシンで `SGX=1 make run` した直後に確認するなら
`test-local` を使う。

正常なら `hello1` のような実行結果が HTTP 200 で返る。

## トラブルシューティング

- `gramine-sgx: No such file or directory` → Gramine 未インストール。手順 1 を実施。
- `Invalid application path specified (wasm-runner.manifest.sgx does not exist)` →
  `SGX=1 make run` の前に `SGX=1 make`（マニフェスト生成）を実行し忘れている。
- `Error: Address in use (os error 98)` → 前回起動した `wasm-runner` プロセスが
  ポート 3000 を掴んだまま残っている。`ps aux | grep gramine` で確認し `kill -9` する。
  `timeout` コマンドで起動した場合、SIGTERM が効かずプロセスが残ることがあるため注意。
- `docker: permission denied` → `usermod -aG docker` 後に再ログインしていない。
  `sudo docker ...` で回避するか、シェルを開き直す。
