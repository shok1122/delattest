# passman — パスワードマネージャ wasm モジュール

登録済みデータとしてサーバに封印されたボルト（パスワード帳）を、サンドボックス内で
読み出し・検索・更新するデモモジュール。更新は「新ボルトを stdout で受け取り、
新データとして登録し直し、旧ボルトを削除証明つきで削除する」というフローになり、
本システムのデータライフサイクル（登録 → 利用 → 証明つき削除）に沿って動く。

## 入力の規約

ボルトは登録済みデータとして `?data=<vault_id>`（1番目）で指定し、コマンドは
**WASI argv**（`?arg=` の繰り返し）で渡すのが基本形。

```
POST /execute?data=<vault_id>&arg=get&arg=github
```

| 入力 | 内容 |
|---|---|
| `/data/input0` | ボルト。1行1エントリの `name<TAB>username<TAB>password`。空行と `#` 始まりの行は無視 |
| argv（`?arg=` の繰り返し） | コマンド。`argv[1]` 以降を空白1個で連結して1行として解釈する |
| `/data/input1`（任意） | コマンド（1行）。argv が無い場合のフォールバック（コマンドを登録済みデータとして渡す旧方式） |

コマンドの優先順は argv > `/data/input1` > `list`（どちらも無い場合）。

## コマンド

| コマンド | 出力（stdout） |
|---|---|
| `list` | `name<TAB>username` の一覧（パスワードは出力しない） |
| `get <name>` | 該当エントリのパスワードのみ |
| `add <name> <username> <password>` | エントリを追加した**新ボルト全体**（パスワードは空白を含んでよい。行末まで） |
| `del <name>` | エントリを除いた**新ボルト全体** |

- エラー（未知のコマンド・エントリなし・`add` の重複など）は stderr に出力され、
  終了コードは 0。runner の応答が `-- stdout -- / -- stderr --` 併記になっていたら
  エラーである（runner は終了コードが 0 以外だと stdout/stderr を捨てるため、
  この規約にしている）。
- パスワード生成コマンドは提供しない。サンドボックス内の乱数は決定的な擬似値で
  あり、安全な乱数源が無いため。
- ボルト自体はモジュール内では暗号化しない。機密性は封印ストレージ
  （encrypted mount + データごとの DEK）が担う。

## ビルド

```sh
cd wasm_module
make build          # wasi-build イメージの作成（初回のみ）
make SRC=passman    # passman/app.wasm を生成
```

## 使ってみる

サーバを起動しておく（リポジトリ直下で `make run`。Gramine を使わずに試すなら
`cd wasm_runner && docker run --rm -v "$PWD":/work -w /work -p 3000:3000 golang:1.25-bookworm go run .`）。
以下は `wasm_module/passman/` で実行する。

### 1. ユーザ発行と初期ボルトの登録

```sh
# APIキーの発行（api_key は応答限りなので控えておく）
curl -s -X POST http://localhost:3000/users
# → {"api_key":"ak-...","created_at":"...","owner_id":"u-..."}
KEY=ak-...   # 上の api_key

# 初期ボルト（タブ区切り）を作って登録
printf 'github\talice\tgh-p@ss-1\nbank\talice\tbank-p@ss-2\n' > vault.txt
curl -s -X POST http://localhost:3000/data -H "X-API-Key: $KEY" --data-binary @vault.txt
# → {"data_id":"d-...","registered_at":"..."}
VID=d-...    # 上の data_id
```

### 2. list（コマンド省略 = ボルトだけを指定）

```sh
curl -s -X POST "http://localhost:3000/execute?data=$VID" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/wasm" --data-binary @app.wasm
# → github  alice
#    bank    alice
```

### 3. get（コマンドは `?arg=` で渡す）

```sh
curl -s -X POST "http://localhost:3000/execute?data=$VID&arg=get&arg=github" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/wasm" --data-binary @app.wasm
# → gh-p@ss-1
```

コマンドは使い捨ての実行パラメータなので、登録（`POST /data`）は不要。
argv が使えない環境向けに、コマンド1行を登録して2番目のデータ
（`&data=<cmd_id>` → `/data/input1`）として渡す旧方式も引き続き動く。

### 4. add — ボルトの更新（新規登録 + 旧ボルトの証明つき削除）

```sh
# 新ボルト全体が stdout に返るのでファイルに受ける
curl -s -X POST "http://localhost:3000/execute?data=$VID&arg=add&arg=gitlab&arg=bob&arg=correct&arg=horse&arg=battery" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/wasm" --data-binary @app.wasm > vault2.txt

# 新ボルトを登録し直す
curl -s -X POST http://localhost:3000/data -H "X-API-Key: $KEY" --data-binary @vault2.txt
VID2=d-...   # 新しい data_id

# 旧ボルトを削除（削除証明 JSON が返る）
curl -s -X DELETE "http://localhost:3000/data/$VID" -H "X-API-Key: $KEY"
# 証明は後からも取得できる: GET /data/$VID/proof

# 以後は VID2 を使う。旧ボルトの実行は 404 になる
curl -s -X POST "http://localhost:3000/execute?data=$VID2" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/wasm" --data-binary @app.wasm
```

`del <name>` も同じ流れ（新ボルトを受けて登録し直し、旧ボルトを削除）。

### Makefile 経由の実行

`wasm_module/` の共通ターゲットでも実行できる:

```sh
cd wasm_module
make test-local SRC=passman DATA=$VID ARGS="get github" KEY=$KEY
```

### エラー応答の例

```sh
curl -s -X POST "http://localhost:3000/execute?data=$VID&arg=get&arg=nonexistent" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/wasm" --data-binary @app.wasm
# -- stdout --
#
#
# -- stderr --
# error: no entry named nonexistent
```

### `?arg=` の注意点

- argv は空白1個で連結して解釈するため、連続空白を含むパスワードは argv 経由では
  正しく渡せない（その場合は旧方式の `/data/input1` を使う）。URL に使えない文字は
  URLエンコードして渡す。
- クエリ文字列はプロキシやアクセスログに残り得る。`add` のようにパスワードが
  コマンドに含まれる操作をログが残る経路越しに行う場合は、旧方式
  （コマンドをデータとして登録して `/data/input1` で渡す）の方が安全。
