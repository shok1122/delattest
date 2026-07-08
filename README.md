# delattest

WASM モジュールを HTTP 経由で受け取り、Gramine-SGX のエンクレーブ内で実行する
サーバー（`wasm-runner`）。動作確認済み（2026-07-06, Ubuntu 22.04 / Azure VM）。

データの**登録・利用・削除のライフサイクル管理**と**削除証明の発行**に対応
（設計は `docs/data-lifecycle-spec.md` を参照。動作確認済み 2026-07-08,
gramine-direct / gramine-sgx（`RA_TYPE=none`）。`RA_TYPE=dcap` は本マシンでは
AESM が error 12 を返しエンクレーブ起動不可＝ホスト側 DCAP 基盤の整備が必要）。

## 構成

```
wasm_runner/        wasm-runner 本体（Go, net/http + wazero）。Dockerでビルド（CGO無効の静的バイナリ）
                    実装仕様書: wasm_runner/SPEC.md
wasm_module/        動作確認用の WASM モジュール（hello1, hello2, readinput など, wasi-sdk でビルド）
docs/               設計仕様書（データライフサイクル管理機能）
Makefile            リポジトリ直下。gramine-manifest / gramine-sgx-sign / 実行を行う
wasm-runner.manifest.template   Gramine マニフェストのテンプレート（リポジトリ管理下）
cache/wasm-runner   実行バイナリの配置場所（.gitignore 対象、ビルドして都度生成）
data_store/         封印ストレージ（.gitignore 対象、実行時に生成。ホスト上では常に暗号化状態）
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

`wasm_runner/` は CGO 無効（`CGO_ENABLED=0`）の静的バイナリとしてビルドする Go
プロジェクト。依存は `go.mod` / `go.sum` としてリポジトリ管理下にあるため、
事前生成の手順は不要。

```sh
cd wasm_runner

# ビルド → バイナリ取り出し → リポジトリ直下の cache/ に配置（root Makefile が参照する場所）
make install

cd ..
```

`make install` は内部で `docker build --target builder`（`make builder`）→
`docker cp` によるバイナリ取り出し（`make extract`）→ `../cache/` への配置、を
まとめて実行する。

### 5. マニフェスト生成・署名・実行

```sh
SGX=1 make          # gramine-manifest → gramine-sgx-sign（wasm-runner.manifest{,.sgx}, .sig を生成）
SGX=1 make run      # gramine-sgx wasm-runner を起動（0.0.0.0:3000 で待受）
```

非 SGX（gramine-direct）で試す場合は `SGX=1` を外すだけでよい。

削除証明に SGX quote（第三者検証可能なリモートアテステーション）を含めるには、
ホストに DCAP スタック（aesmd, quote provider）がある環境で
`SGX=1 RA_TYPE=dcap make` としてマニフェストを生成する（既定は `RA_TYPE=none`）。

注意:

- `SGX=1` の有無や `RA_TYPE` を切り替えるときは、`make clean` してから
  マニフェストを再生成すること（マニフェストの内容がモードによって変わるため）。
- 封印ストレージの鍵は SGX では MRSIGNER 由来のシーリング鍵、gramine-direct では
  開発用固定鍵を使う。**モードを跨いで同じ `data_store/` は読めない**ので、
  切り替え時は `data_store/` を空にすること。

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

## API

| メソッド / パス | 説明 |
|---|---|
| `GET /` | ヘルスチェック |
| `POST /execute-wasm` | ステートレス実行（動作確認用）。ライフサイクル管理・削除証明の対象外 |
| `POST /data` | データ登録。ボディ＝データ本体。応答: `{ data_id, registered_at }` |
| `POST /data/{id}/execute` | 登録済みデータに対する WASM 実行。ボディ＝WASMバイナリ。応答: 実行結果 |
| `DELETE /data/{id}` | データ削除。応答: 削除証明（JSON） |
| `GET /data/{id}/status` | 現在の状態（`REGISTERED`/`IN_USE`/`DELETING`/`DELETED`）。生データは返さない |
| `GET /data/{id}/proof` | `DELETED` のデータの削除証明を再取得（監査用） |

登録時に `X-Owner-Token: <token>`（または `Authorization: Bearer <token>`）を付けると、
以後その ID への `execute`/`delete` に同じトークンが要求される（トークンの発行・認可
プロトコルの設計は仕様書 §11 の未解決課題）。

### 使用例（ライフサイクル一巡）

```sh
# 登録（トークン付き）
curl -X POST http://localhost:3000/data \
  -H "X-Owner-Token: my-secret" \
  --data-binary "sensitive user data"
# => {"data_id":"d-xxxxxxxxxxxxxxxx","registered_at":"..."}

# 登録済みデータに対して WASM を実行。データは WASM から /data/input として
# 読み取り専用で見える（readinput はそれを stdout に書き出すサンプル）
make -C wasm_module/readinput all
curl -X POST http://localhost:3000/data/d-xxxxxxxxxxxxxxxx/execute \
  -H "X-Owner-Token: my-secret" \
  --data-binary @wasm_module/readinput/app.wasm

# 削除 → 削除証明（JSON）が返る
curl -X DELETE http://localhost:3000/data/d-xxxxxxxxxxxxxxxx \
  -H "X-Owner-Token: my-secret"

# 削除証明の再取得（監査用、トークン不要）
curl http://localhost:3000/data/d-xxxxxxxxxxxxxxxx/proof
```

### 削除証明

削除時に発行される JSON。`enclave_report.quote` は起動時に一度だけ生成される
SGX quote で、`user_report_data` に署名公開鍵の SHA-256 が埋め込まれている。
検証者は quote を DCAP で検証 → quote 内の公開鍵ハッシュと `public_key` の一致を
確認 → `signature`（`{data_id, deleted_at, content_hash}` をこの順に並べた JSON への
Ed25519 署名）を検証 → `content_hash` を登録データの sha256 と照合する
（仕様書 §9.3）。`RA_TYPE=none` や gramine-direct では quote は空になり、
署名のみの証明となる（開発用）。

### 実行制約（サンドボックス）

- ネットワーク系のホスト関数は一切提供しない（WASM からの外部送信は不可能）
- ファイルシステムは実行対象データ 1 件が `/data/input` に読み取り専用で見えるのみ
  （メモリ上のFSであり、平文がホストのディスクに書かれることはない）
- 実行タイムアウト: `EXEC_TIMEOUT_SEC`（既定 30 秒）
- メモリ上限: `WASM_MEM_LIMIT_PAGES`（既定 1024 ページ = 64 MiB）
- stdout/stderr はそれぞれ 1 MiB まで（DoS 対策。超過分は切り捨て）

### 環境変数

| 変数 | 既定値 | 説明 |
|---|---|---|
| `HOST` / `PORT` | `0.0.0.0` / `3000` | 待ち受けアドレス |
| `DATA_DIR` | `data_store` | 封印ストレージの場所（Gramine 実行時はマニフェストが `/data_store` を設定） |
| `EXEC_TIMEOUT_SEC` | `30` | WASM 実行タイムアウト（秒） |
| `WASM_MEM_LIMIT_PAGES` | `1024` | WASM メモリ上限（64 KiB ページ数） |

## テスト

`wasm_runner/` の Go テスト（状態遷移・排他制御・永続化・クラッシュリカバリ・
署名検証・実行制約）は Docker で実行できる:

```sh
cd wasm_runner
docker run --rm -v "$PWD":/work -w /work golang:1.25-bookworm go test ./... -count=1
```

`testdata/readinput.wasm`（`/data/input` 読み取りのテスト用フィクスチャ）は
`wasm_module/readinput/` のビルド成果物のコピー。

## トラブルシューティング

- `gramine-sgx: No such file or directory` → Gramine 未インストール。手順 1 を実施。
- `Invalid application path specified (wasm-runner.manifest.sgx does not exist)` →
  `SGX=1 make run` の前に `SGX=1 make`（マニフェスト生成）を実行し忘れている。
- `Error: Address in use (os error 98)` → 前回起動した `wasm-runner` プロセスが
  ポート 3000 を掴んだまま残っている。`ps aux | grep gramine` で確認し `kill -9` する。
  `timeout` コマンドで起動した場合、SIGTERM が効かずプロセスが残ることがあるため注意。
- `docker: permission denied` → `usermod -aG docker` 後に再ログインしていない。
  `sudo docker ...` で回避するか、シェルを開き直す。
