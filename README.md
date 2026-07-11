# delattest

WASM モジュールを HTTP 経由で受け取り、Gramine-SGX のエンクレーブ内で実行する
サーバー（`wasm-runner`）。動作確認済み（2026-07-06, Ubuntu 22.04 / Azure VM）。

データの**登録・利用・削除のライフサイクル管理**と**削除証明の発行**に対応
（設計は `docs/260709-data-lifecycle-spec.md` を参照。動作確認済み 2026-07-08,
gramine-direct / gramine-sgx（`RA_TYPE=none`）。`RA_TYPE=dcap` は本マシンでは
AESM が error 12 を返しエンクレーブ起動不可＝ホスト側 DCAP 基盤の整備が必要）。

2026-07-10 に **Ed25519 署名認証（API キー廃止・オーナー鍵の TOFU 登録）**、
**WASM プログラムの事前アップロード（`POST /programs`、コンテンツアドレス ID）**、
**データごとの実行可能プログラムホワイトリスト**を導入した（計画書:
`docs/260710-signature-auth-and-program-upload-workplan.md`、実装仕様:
`wasm_runner/SPEC.md`。非 Gramine のローカル実行で一巡確認済み、Gramine 実機は未確認。
旧 `data_store/` とは非互換のため、更新時は `data_store/` を空にして登録し直す）。

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

最初にオーナー鍵を生成し、**サーバ起動直後に**公開鍵を登録する（TOFU: 初回のみ。
未登録の間はコマンドがすべて 403 で拒否される）:

```sh
scripts/runner-cli.sh keygen owner.pem
scripts/runner-cli.sh -k owner.pem owner-register --wait 30
```

`wasm_module/` 側で WASM モジュールをビルドしてサーバーに登録・実行する:

```sh
cd wasm_module

# wasi-sdk ビルド環境のイメージを作成（初回のみ）
docker build -t wasi-build .

# hello1 をビルド（main.c → app.wasm）
make -C hello1 all

# アップロードして実行（このマシンで動かしている wasm-runner 宛て: localhost:3000）
make test-local SRC=hello1 KEY=../owner.pem

# もしくは本番ドメイン (delattest.dev:3000) で稼働しているサーバー宛て
make test SRC=hello1 KEY=../owner.pem
```

`test` と `test-local` の違いは送信先ホストだけ（`wasm_module/Makefile` 参照）。
どちらも「`POST /programs` へのアップロード（冪等）→ `POST /execute`」を行う。

| ターゲット | 送信先 |
|---|---|
| `test-local` | `http://localhost:3000` — 手元のマシンで起動した `wasm-runner` を直接叩く |
| `test` | `http://delattest.dev:3000` — 本番ドメインで稼働中のサーバーを叩く |

このセットアップ手順どおりに自分のマシンで `SGX=1 make run` した直後に確認するなら
`test-local` を使う。

正常なら `hello1` のような実行結果が HTTP 200 で返る。

## API

認証は **Ed25519 リクエスト署名**（2026-07-10 に API キー方式から全面移行）。
コマンド（状態を変える操作）は `X-Public-Key` / `X-Signature` / `X-Timestamp` ヘッダで
`<METHOD>\n<PATH>\n<timestamp>\nsha256(body)` への署名を検証する。手作業で curl を
組むのは大変なので、署名付き送信は `scripts/runner-cli.sh` を使う（下の使用例）。
詳細仕様は `wasm_runner/SPEC.md` §4。

| メソッド / パス | 説明 |
|---|---|
| `GET /` | ヘルスチェック（認証不要） |
| `POST /owner` | オーナー公開鍵の登録（**TOFU: 未登録時の初回のみ**。登録済みは 409）。ボディ＝JSON `{"public_key":"<base64>"}`。未登録の間はコマンドがすべて 403 |
| `POST /programs` | WASM プログラムの事前アップロード（**オーナー署名**）。ボディ＝JSON `{"wasm":"<base64>"}`。応答: `{ program_id, uploaded_at }`。`program_id` は `p-<sha256(バイナリ)>` のコンテンツアドレスで、同一バイナリの再アップロードは冪等（200） |
| `DELETE /programs/{id}` | プログラム削除（**オーナー署名**）。再アップロードで同じ ID に復元可能 |
| `POST /execute` | WASM 実行（**オーナー署名**。ステートレス実行も含む）。ボディ＝JSON `{"program_id":"p-...","data":["<id>",...],"args":["<v>",...]}`（`data`/`args` は省略可）。指定順の i 番目のデータが `/data/input<i>` として WASM から見える。指定した各データのホワイトリストに `program_id` が含まれていなければ 403。応答: 実行結果 |
| `POST /data` | データ登録（**任意の Ed25519 鍵の署名**。事前のユーザ登録不要）。ボディ＝JSON `{"data":"<base64>","allowed_programs":["p-...",...]}`。署名した公開鍵がアップローダとして記録される。`allowed_programs` 省略時は**空＝すべて拒否**（deny by default） |
| `PUT /data/{id}/programs` | ホワイトリストの全置換（**記録済みアップローダ鍵の署名のみ**。オーナー鍵では不可） |
| `DELETE /data/{id}` | データ削除（**オーナー鍵または記録済みアップローダ鍵の署名**）。応答: 削除証明（JSON） |
| `GET /data/{id}/status` | 現在の状態（`REGISTERED`/`IN_USE`/`DELETING`/`DELETED`）。生データは返さない（認証不要） |
| `GET /data/{id}/proof` | `DELETED` のデータの削除証明を再取得（監査用、認証不要） |

`execute` で指定した全データは実行中 `IN_USE` になる。取得は all-or-nothing で、
1件でも存在しない・削除済み・使用中・ホワイトリスト外の場合は（対象のデータIDを
エラーメッセージに含めて）実行せず、他のデータの状態も変更しない。同一IDの重複指定は 400。

### 使用例（ライフサイクル一巡）

```sh
CLI=scripts/runner-cli.sh    # 依存は curl / openssl 3.x / coreutils のみ

# 鍵ペアの生成（オーナー用・アップローダ用。同一人物なら1つでよい）
$CLI keygen owner.pem
$CLI keygen uploader.pem

# デプロイ直後にオーナー公開鍵を登録（TOFU。起動→登録は一連の手順で行うこと）
$CLI -k owner.pem owner-register --wait 30

# WASM プログラムの事前アップロード（オーナー鍵）。program_id が返る
# （readinput は input0, input1, ... を順に連結して stdout に書き出すサンプル）
make -C wasm_module/readinput all
$CLI -k owner.pem program-upload wasm_module/readinput/app.wasm
# => {"program_id":"p-<sha256>","uploaded_at":"..."}    ※ ID は手元でも計算できる:
$CLI program-id wasm_module/readinput/app.wasm

# データ登録（アップローダ鍵）。実行を許可するプログラムをホワイトリストで指定
echo -n "sensitive user data" > input.bin
$CLI -k uploader.pem data-upload input.bin p-<sha256>
# => {"data_id":"d-xxxxxxxxxxxxxxxx","registered_at":"..."}

# 実行（オーナー鍵）。-d は複数指定可、-a は WASI argv
$CLI -k owner.pem execute p-<sha256> -d d-xxxxxxxxxxxxxxxx

# ホワイトリストの変更（アップローダ鍵のみ。全置換、引数なしで「すべて拒否」）
$CLI -k uploader.pem data-programs d-xxxxxxxxxxxxxxxx p-<sha256> p-<別のhash>

# 削除（オーナー鍵またはアップローダ鍵）→ 削除証明（JSON）が返る
$CLI -k uploader.pem data-delete d-xxxxxxxxxxxxxxxx

# 削除証明の再取得（監査用、認証不要）
$CLI proof d-xxxxxxxxxxxxxxxx
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
| `AUTH_WINDOW_SEC` | `300` | 署名タイムスタンプの許容ウィンドウ（秒、リプレイ対策） |
| `EXEC_TIMEOUT_SEC` | `30` | WASM 実行タイムアウト（秒） |
| `WASM_MEM_LIMIT_PAGES` | `1024` | WASM メモリ上限（64 KiB ページ数） |

## リセット手順（クリーンな状態に戻す）

オーナー鍵を TOFU から登録し直したいとき、`SGX=1` / `RA_TYPE` のモードを
切り替えるとき、非互換の更新を取り込んだときは、以下でサーバ側の状態を
すべて消去して最初からやり直す。

```sh
# 1. サーバの停止（起動中の場合）
ps aux | grep gramine     # PID を確認して kill -9

# 2. 封印ストレージの中身を全消去
#    users.json（登録済みオーナー公開鍵）を消さない限り、オーナーの再登録は
#    409 で拒否され続ける。TOFU をやり直すには全消去が必要
rm -rf data_store/blobs data_store/meta data_store/users.json

# 3. 生成済みマニフェストの削除（モードを選び直して再生成できるように）
make clean
```

`cache/wasm-runner`（ビルド済みバイナリ）はそのまま使い回せるので消さなくてよい
（消してしまった場合は手順 4 で再ビルド）。手元の鍵ファイル（`owner.pem` など）も
削除不要で、再利用しても新しく `keygen` し直してもよい。

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
