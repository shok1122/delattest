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
| `POST /users` | ユーザ発行。応答: `{ owner_id, api_key, created_at }`。APIキーの平文はこの応答限り（サーバはハッシュのみ保存） |
| `POST /execute` | WASM 実行。ボディ＝JSON `{"wasm":"<base64>","data":["<id>",...],"args":["<v>",...]}`（`data`/`args` は省略可）。`data` は使用する登録済みデータのID（**0個以上・可変長**）で、指定順の i 番目が `/data/input<i>` として WASM から見える。`args` は WASI argv（使い捨ての実行パラメータで、ライフサイクル管理の対象外）。`data` を1個以上指定する場合は認証必須。`data` 指定なしはステートレス実行（ライフサイクル管理・削除証明の対象外、認証不要）。応答: 実行結果 |
| `POST /data` | データ登録（認証必須）。ボディ＝データ本体。応答: `{ data_id, registered_at }` |
| `DELETE /data/{id}` | データ削除（認証必須、所有者本人のみ）。応答: 削除証明（JSON） |
| `GET /data/{id}/status` | 現在の状態（`REGISTERED`/`IN_USE`/`DELETING`/`DELETED`）。生データは返さない |
| `GET /data/{id}/proof` | `DELETED` のデータの削除証明を再取得（監査用） |

認証は APIキーを `X-API-Key: <key>`（または `Authorization: Bearer <key>`）で渡す。
`POST /users` が返す `owner_id` はユーザの恒久的な識別子（秘密ではない）、`api_key` は
それを証明する秘密で、登録したデータには owner_id が所有者として記録される。
`execute`/`delete` は「認証済みユーザ＝データの所有者本人」の場合のみ許可され（不一致は
403）、`execute` で複数データを指定する場合は全データが自分のものでなければならない。
キー未提示・無効なキーは 401（ユーザ発行エンドポイント自体の保護・キーのローテーション
は仕様書 §11 の未解決課題）。

`execute` で指定した全データは実行中 `IN_USE` になる。取得は all-or-nothing で、
1件でも存在しない・削除済み・使用中・所有者不一致の場合は（対象のデータIDを
エラーメッセージに含めて）実行せず、他のデータの状態も変更しない。同一IDの重複指定は 400。

### 使用例（ライフサイクル一巡）

```sh
# ユーザ発行。api_key はこの応答でしか得られないので控えておく
curl -X POST http://localhost:3000/users
# => {"owner_id":"u-xxxxxxxxxxxxxxxx","api_key":"ak-...","created_at":"..."}

# 登録（認証必須。登録したデータは自分の owner_id に紐づく）
curl -X POST http://localhost:3000/data \
  -H "X-API-Key: ak-..." \
  --data-binary "sensitive user data"
# => {"data_id":"d-xxxxxxxxxxxxxxxx","registered_at":"..."}

# 登録済みデータに対して WASM を実行（所有者本人のみ）。JSON ボディの data で
# 指定した i 番目のデータが WASM から /data/input<i> として読み取り専用で見える
# （readinput は input0, input1, ... を順に連結して stdout に書き出すサンプル）
make -C wasm_module/readinput all
printf '{"wasm":"%s","data":["d-xxxxxxxxxxxxxxxx"]}' \
    "$(base64 -w0 wasm_module/readinput/app.wasm)" \
  | curl -X POST http://localhost:3000/execute \
      -H "X-API-Key: ak-..." -H "Content-Type: application/json" \
      --data-binary @-

# 複数データを使う場合は data の配列に並べる（全データが自分のものであること。
# 0個ならステートレス実行で、認証不要）。args を付けると WASI argv として渡る
printf '{"wasm":"%s","data":["d-xxxxxxxxxxxxxxxx","d-yyyyyyyyyyyyyyyy"],"args":["get","github"]}' \
    "$(base64 -w0 wasm_module/readinput/app.wasm)" \
  | curl -X POST http://localhost:3000/execute \
      -H "X-API-Key: ak-..." -H "Content-Type: application/json" \
      --data-binary @-

# 削除（所有者本人のみ）→ 削除証明（JSON）が返る
curl -X DELETE http://localhost:3000/data/d-xxxxxxxxxxxxxxxx \
  -H "X-API-Key: ak-..."

# 削除証明の再取得（監査用、認証不要）
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
- ファイルシステムは実行時に指定した登録データが指定順に `/data/input0`,
  `/data/input1`, ... へ読み取り専用で見えるのみ
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

`testdata/readinput.wasm`（`/data/input0`, `/data/input1`, ... 読み取りのテスト用
フィクスチャ）は `wasm_module/readinput/` のビルド成果物のコピー。

## トラブルシューティング

- `gramine-sgx: No such file or directory` → Gramine 未インストール。手順 1 を実施。
- `Invalid application path specified (wasm-runner.manifest.sgx does not exist)` →
  `SGX=1 make run` の前に `SGX=1 make`（マニフェスト生成）を実行し忘れている。
- `Error: Address in use (os error 98)` → 前回起動した `wasm-runner` プロセスが
  ポート 3000 を掴んだまま残っている。`ps aux | grep gramine` で確認し `kill -9` する。
  `timeout` コマンドで起動した場合、SIGTERM が効かずプロセスが残ることがあるため注意。
- `docker: permission denied` → `usermod -aG docker` 後に再ログインしていない。
  `sudo docker ...` で回避するか、シェルを開き直す。
