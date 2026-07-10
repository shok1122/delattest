# passman — パスワードマネージャ wasm モジュール

登録済みデータとしてサーバに封印されたボルト（パスワード帳）を、サンドボックス内で
読み出し・検索・更新するデモモジュール。更新は「新ボルトを stdout で受け取り、
新データとして登録し直し、旧ボルトを削除証明つきで削除する」というフローになり、
本システムのデータライフサイクル（登録 → 利用 → 証明つき削除）に沿って動く。

## 入力の規約

`POST /execute` の JSON ボディで、ボルトの data_id を `data` の1番目に、
コマンドを `args`（WASI argv）に指定するのが基本形。

```json
{"wasm": "<base64 エンコードした app.wasm>", "data": ["<vault_id>"], "args": ["get", "github"]}
```

| 入力 | 内容 |
|---|---|
| `/data/input0` | ボルト。1行1エントリの `name<TAB>username<TAB>password`。空行と `#` 始まりの行は無視 |
| argv（`args` 配列） | コマンド。`argv[1]` 以降を空白1個で連結して1行として解釈する |
| `/data/input1`（任意） | コマンド（1行）。argv が無い場合のフォールバック（コマンドを登録済みデータとして `data` の2番目に指定する旧方式） |

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

実行リクエストは毎回同じ形（JSON ボディに base64 の WASM を入れる）なので、
シェル関数を用意しておくと楽:

```sh
# 使い方: exec_wasm '["<data_id>", ...]' '["<arg>", ...]'（どちらも省略可）
exec_wasm() {
  printf '{"wasm":"%s","data":%s,"args":%s}' \
      "$(base64 -w0 app.wasm)" "${1:-[]}" "${2:-[]}" \
    | curl -s -X POST http://localhost:3000/execute \
        -H "X-API-Key: $KEY" -H "Content-Type: application/json" --data-binary @-
}
```

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
exec_wasm "[\"$VID\"]"
# → github  alice
#    bank    alice
```

### 3. get（コマンドは `args` で渡す）

```sh
exec_wasm "[\"$VID\"]" '["get","github"]'
# → gh-p@ss-1
```

コマンドは使い捨ての実行パラメータなので、登録（`POST /data`）は不要。
コマンド1行を登録して2番目のデータ（`data` の2番目 → `/data/input1`）として
渡す旧方式も引き続き動く。

### 4. add — ボルトの更新（新規登録 + 旧ボルトの証明つき削除）

```sh
# 新ボルト全体が stdout に返るのでファイルに受ける
exec_wasm "[\"$VID\"]" '["add","gitlab","bob","correct","horse","battery"]' > vault2.txt

# 新ボルトを登録し直す
curl -s -X POST http://localhost:3000/data -H "X-API-Key: $KEY" --data-binary @vault2.txt
VID2=d-...   # 新しい data_id

# 旧ボルトを削除（削除証明 JSON が返る）
curl -s -X DELETE "http://localhost:3000/data/$VID" -H "X-API-Key: $KEY"
# 証明は後からも取得できる: GET /data/$VID/proof

# 以後は VID2 を使う。旧ボルトの実行は 404 になる
exec_wasm "[\"$VID2\"]"
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
exec_wasm "[\"$VID\"]" '["get","nonexistent"]'
# -- stdout --
#
#
# -- stderr --
# error: no entry named nonexistent
```

### `args` の注意点

- argv は空白1個で連結して解釈するため、連続空白を含むパスワードは argv 経由では
  正しく渡せない（その場合は旧方式の `/data/input1` を使う）。
- JSON 文字列なので、パスワードに `"` や `\` を含める場合は JSON エスケープが必要。
