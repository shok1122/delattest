# wasm_runner 実装仕様書（as-built）

本書は `wasm_runner` に実装したデータライフサイクル管理機能の**実装仕様**である。
設計仕様書 `docs/260709-data-lifecycle-spec.md`（以下「設計書」、§n は設計書の章番号）
および作業計画書 `docs/260710-signature-auth-and-program-upload-workplan.md`
（以下「計画書」）に対して、実際のコードが何をどう実装しているかを記述する。
実装レビュー時は本書とコードを突き合わせて確認できるよう、各節に実装箇所
（ファイル・関数名）を記す。

検証状況: ユニットテスト40件 + race detector + go vet パス（2026-07-10）。
Ed25519 署名認証・プログラムレジストリ・ホワイトリスト（計画書 U1–U5、2026-07-10
導入）は非 Gramine のローカル実行で `scripts/runner-cli.sh` による一巡
（TOFU 登録 → program 登録 → data 登録 → execute → ホワイトリスト編集 → delete →
proof → 再起動後の永続性）を確認済み。Gramine 実機（direct / SGX）は本改修後は未確認。

## 1. ファイル構成と責務

| ファイル | 責務 | 対応文書 |
|---|---|---|
| `main.go` | 起動処理（設定読み込み → store → owner → auth → programs → prover → lifecycleManager → HTTPサーバ）、graceful shutdown | — |
| `auth.go` | Ed25519 署名認証（署名対象メッセージの正規化・検証・タイムスタンプウィンドウ・リプレイ対策） | 計画書 §3.1 |
| `owner.go` | オーナー鍵の TOFU 登録・永続化（Owner Manager） | 計画書 §3.1 案B |
| `programs.go` | プログラムレジストリ（コンテンツアドレス ID・冪等 put・整合性検証付き get・delete） | 計画書 §3.2 |
| `lifecycle.go` | 状態機械・メタデータ管理・排他制御・ホワイトリスト照合/編集・クラッシュリカバリ（Lifecycle Manager） | 設計書 §5, §6.1、計画書 §3.3–3.4 |
| `storage.go` | 封印ストレージのファイル配置・AES-256-GCM 暗号化・原子的書き込み | 設計書 §6.2, §9.1 |
| `sandbox.go` | WASM 実行サンドボックス（wazero、実行制約） | 設計書 §6.3, §8 |
| `attestation.go` | 削除証明用署名鍵の生成・SGX quote/レポート取得・削除証明の発行 | 設計書 §6.4, §9 |
| `handlers.go` | HTTP API ルーティング・リクエスト検証・認可・エラー応答 | 設計書 §7 |
| `*_test.go` | ユニットテスト（§12 参照） | — |
| `testdata/readinput.wasm` | `/data/input0`, `/data/input1`, ... を順に連結出力するテスト用フィクスチャ（`wasm_module/readinput/` のビルド成果物） | — |
| `../scripts/runner-cli.sh` | クライアント補助（鍵生成・署名付きリクエスト・デプロイ手順。§4.2） | 計画書 T10 |

`users.go`（API キー方式のユーザ管理）は 2026-07-10 に**廃止**した（§10）。

## 2. 起動シーケンス（`main.go: main`）

1. `HOST`/`PORT` から待ち受けアドレスを決定。
2. `newStore(DATA_DIR)` — `meta/`・`blobs/`・`programs/` サブディレクトリを作成（0700）。
3. `newOwnerManager(store)` — 登録済みオーナー鍵（`owner.json`）があればロード。
   未登録ならその旨をログに出し、`POST /owner` による初回登録を待つ
   （登録までコマンドはすべて 403）。
4. `newAuthenticator(AUTH_WINDOW_SEC)` — 署名認証（リプレイ対策ウィンドウ）の初期化。
5. `newProgramRegistry(store)` — 永続化済みプログラムメタデータ（`programs/*.json`）をロード。
6. `newProver()` — 削除証明用の Ed25519 鍵ペアを生成し、`/dev/attestation/*` から
   quote またはローカルレポートを取得（取得できない環境では署名のみのモードに縮退）。
7. `newLifecycleManager(store, prover)` — 永続化済みメタデータを全件ロードし、
   中断状態をリカバリ。リカバリ失敗時は起動エラーで停止。
8. サンドボックス設定（`EXEC_TIMEOUT_SEC`, `WASM_MEM_LIMIT_PAGES`）を読み込み、
   HTTP サーバを起動。SIGINT/SIGTERM で 5 秒タイムアウトの graceful shutdown。

## 3. 設定

### 環境変数（`main.go: getEnv/envInt`）

| 変数 | 既定値 | 説明 |
|---|---|---|
| `HOST` / `PORT` | `0.0.0.0` / `3000` | 待ち受けアドレス |
| `DATA_DIR` | `data_store` | 封印ストレージの場所。Gramine 実行時はマニフェストの `loader.env.DATA_DIR = "/data_store"`（encrypted mount）が優先される |
| `AUTH_WINDOW_SEC` | `300` | 署名タイムスタンプの許容ウィンドウ（秒、過去・未来とも）。Gramine 実行時に変えたい場合はマニフェストの `loader.env` への追加が必要 |
| `EXEC_TIMEOUT_SEC` | `30` | WASM 実行タイムアウト（秒、正の整数を想定） |
| `WASM_MEM_LIMIT_PAGES` | `1024` | WASM メモリ上限（64 KiB ページ数）。1〜65536 の範囲外は起動時エラー |

### 定数（`handlers.go`）

| 定数 | 値 | 説明 |
|---|---|---|
| `maxDataBytes` | 32 MiB | 登録データ（base64 デコード後）の上限 |
| `maxWasmBytes` | 32 MiB | WASM バイナリ（base64 デコード後）の上限 |
| `maxArgsBytes` | 8 KiB | `POST /execute` の引数（`args`）の合計上限 |
| `maxUploadBodyBytes` | 44 MiB | `POST /programs`・`POST /data` の JSON ボディ上限（base64 で約 4/3 倍に膨れた 32 MiB + 余白） |
| `maxExecBodyBytes` | 1 MiB | `POST /execute` の JSON ボディ上限（WASM 本体が `program_id` 参照になったため小さい） |
| `maxSmallBodyBytes` | 1 MiB | `POST /owner`・`PUT /data/{id}/programs`・DELETE 系の JSON ボディ上限 |
| `outputCap` | 1 MiB | `sandbox.go`。stdout / stderr それぞれの上限（超過分は切り捨て） |

リクエストボディは全量をメモリに読み込む。エンクレーブサイズ（現状 2G）との
バランスで決めた値。

## 4. HTTP API 詳細（`handlers.go`）

ルーティングは Go 1.22+ の `http.ServeMux` パターン。未定義パスは 404、
メソッド不一致は 405 を返す。エラー応答はすべて JSON `{"error":"<メッセージ>"}`。
text/plain を返すのは `GET /`（ヘルスチェック）と実行結果（`POST /execute` 成功時）のみ。

**クエリパラメータは全 API で使用しない**（計画書 §7）。リクエストのパラメータは
すべて JSON ボディに含める。JSON ボディは**未知フィールドを拒否**する厳密デコード
（`decodeJSON`: `DisallowUnknownFields` + 後続データ拒否）で、旧 API のフィールド
（execute の `wasm` 等）が黙って無視される事故を防ぐ。

### 4.1 認証と認可（`auth.go` / `owner.go` / 各ハンドラ）

API キー方式（`POST /users` / `users.json` / `X-API-Key`）は**廃止**し、すべての
コマンド（状態を変える操作）を **Ed25519 署名**で認証する（計画書 U1/U3/U4）。

**オーナー鍵の登録（TOFU、計画書 §3.1 案B）**

- wasm_runner は「1ユーザ（＝ランナーオーナー）が自身のためにデプロイするサーバ」
  であり、デプロイ直後に `POST /owner` で**オーナーの Ed25519 公開鍵を1つ登録**する。
- 登録は未登録のときの初回のみ受け付け（登録済みは 409）、以後変更不可。
  `owner.json` として封印ストレージに永続化される（`ownerManager`）。
- **未登録の間はコマンドをすべて 403 で拒否**する（`authenticateCommand` の前段。
  未登録状態でコマンドが通る窓を作らない）。認証不要 API（`GET /` と
  `status`/`proof`）は未登録でも応答する。
- 初回登録リクエストを攻撃者に先取りされるリスクへの対策は「デプロイ直後に即座に
  登録する」運用（`runner-cli.sh owner-register --wait`。§4.2）。オーナー鍵は
  マニフェスト計測に含まれないため、アテステーションで検証できるのはランナーの
  コードまで（受容済みのトレードオフ、計画書 §7-Q1）。

**署名プロトコル（`auth.go`）**

コマンドのリクエストに次のヘッダを付与する:

```
X-Public-Key: <Ed25519 公開鍵 raw 32B の base64>
X-Signature:  <署名の base64>
X-Timestamp:  <RFC3339 UTC>
```

署名対象メッセージ（`signedMessage`）は決定的に再構築できる形に正規化する:

```
<METHOD>\n<PATH>\n<X-Timestamp>\n<sha256(request body) の hex 小文字>
```

- `<PATH>` はパスのみ（クエリパラメータは全 API で不使用）。パラメータはすべて
  ボディに載るため、ボディハッシュを通じて署名で改ざんから保護される。
- ボディ無しのリクエスト（DELETE 等）は空バイト列のハッシュを用いる。
- ボディ全体でなくハッシュに署名することで、大きなボディ（最大 44 MiB）でも
  署名検証コストは一定。
- 検証は Go 標準 `crypto/ed25519`（追加依存なし）。検証順はヘッダ形式 →
  タイムスタンプウィンドウ → 署名 → リプレイ。

**リプレイ対策（計画書 §7-Q2: (a) timestamp 方式）**

- `X-Timestamp` が現在時刻から `AUTH_WINDOW_SEC`（既定 300 秒）以内（過去・未来とも）
  でなければ 401。
- 検証済み署名は使用済みキャッシュ（`authenticator.seen`、公開鍵|署名の canonical
  base64 をキーとするメモリ上の map）に 2×ウィンドウの間保持し、同一署名の再提示を
  401 で拒否する。エントリは各認証時に期限切れ分を掃除する。
- キャッシュはメモリ上のみ（再起動で消えるが、ウィンドウ経過後は timestamp 検査が
  拒否するため、再起動を跨ぐリプレイ許容時間は最大でウィンドウ幅）。
- Ed25519 署名は決定的なため、**同一内容のコマンドを同一タイムスタンプで再送すると
  正当なリクエストでもリプレイと判定される**。クライアントはナノ秒精度の RFC3339
  （`RFC3339Nano`。`time.Parse(time.RFC3339, ...)` で受理される）を使うこと
  （`runner-cli.sh`・テストヘルパはそうしている）。
- TEE 内の時計はホスト由来で信頼できない点は既知の制約として受容（計画書 §7-Q2。
  脅威評価に応じて将来サーバ発行 nonce 方式を検討）。

**認証・認可の適用範囲**

| API | 要求される署名 | 実装 |
|---|---|---|
| `POST /owner` | 不要（TOFU。未登録時の初回のみ受理、登録済みは 409） | `handleRegisterOwner` |
| `POST /programs` | オーナー鍵 | `requireOwner` |
| `DELETE /programs/{id}` | オーナー鍵 | `requireOwner` |
| `POST /execute` | オーナー鍵（ステートレス実行も含めて統一、計画書 §7-Q4） | `requireOwner` |
| `POST /data` | **任意の Ed25519 鍵**（署名者がアップローダとして記録される） | `authenticateCommand` |
| `PUT /data/{id}/programs` | 当該データに**記録済みのアップローダ鍵**（オーナー鍵では不可） | `authenticateCommand` + `setAllowedPrograms` 内の照合 |
| `DELETE /data/{id}` | オーナー鍵**または**記録済みアップローダ鍵（計画書 §7-Q3） | `authenticateCommand` + `delete` 内の照合 |
| `GET /`・`status`・`proof` | 不要（生データ・鍵を含まないため） | — |

認証（署名が提示された公開鍵で正当か＝401）と認可（その鍵に当該操作が許されるか＝403）
を分離している。鍵の一致比較はデコード済みバイト列を再エンコードした canonical な
base64 文字列同士で行う（`authenticate`/`ownerManager.register` が正規化する）。

### 4.2 エンドポイント一覧

| メソッド / パス | 成功応答 | エラー |
|---|---|---|
| `GET /` | 200 text（エンドポイント一覧） | — |
| `POST /owner`（ボディ: `{"public_key":"<base64>"}`） | **201** JSON `{"public_key","registered_at"}` | 400（形式不正）/ 409（登録済み）/ 413 |
| `POST /programs`（ボディ: `{"wasm":"<base64>"}`） | **201**（新規）/ **200**（既存＝冪等） JSON `{"program_id","uploaded_at"}` | 400 / 401 / 403 / 413 / 500 |
| `DELETE /programs/{id}` | 200 JSON `{"program_id"}` | 401 / 403 / 404 / 500 |
| `POST /execute`（ボディ: §4.3） | 200 text（実行結果） | 400 / 401 / 403 / 404 / 409 / 413 / 500 |
| `POST /data`（ボディ: `{"data":"<base64>","allowed_programs":["p-...",...]}`） | **201** JSON `{"data_id","registered_at"}` | 400 / 401 / 403 / 413 / 500 |
| `PUT /data/{id}/programs`（ボディ: `{"allowed_programs":[...]}`） | 200 JSON `{"data_id","allowed_programs"}` | 400 / 401 / 403 / 404 / 409 / 500 |
| `DELETE /data/{id}` | 200 JSON（削除証明） | 401 / 403 / 404 / 409 / 500 |
| `GET /data/{id}/status` | 200 JSON | 404 |
| `GET /data/{id}/proof` | 200 JSON（削除証明） | 404 / 409 |

クライアント補助スクリプト `scripts/runner-cli.sh`（bash + curl + openssl 3.x のみに
依存）が全コマンドの署名付き送信・鍵生成・program_id のローカル計算を提供する:

```sh
scripts/runner-cli.sh keygen owner.pem
make run &                                           # または SGX=1 make run
scripts/runner-cli.sh -k owner.pem owner-register --wait 30   # デプロイ直後に即登録
scripts/runner-cli.sh -k owner.pem program-upload app.wasm    # → p-<sha256>
scripts/runner-cli.sh -k uploader.pem data-upload file.bin p-<hash>
scripts/runner-cli.sh -k owner.pem execute p-<hash> -d d-<id> -a arg1
scripts/runner-cli.sh -k uploader.pem data-programs d-<id> p-<hash1> p-<hash2>
scripts/runner-cli.sh -k uploader.pem data-delete d-<id>
```

### 4.3 プログラムレジストリと WASM 実行

**`POST /programs`（`handleUploadProgram` / `programs.go`）**

- ボディ＝JSON `{"wasm":"<base64>"}`（必須・非空・デコード後 32 MiB 以下）。
- **program_id は `p-<sha256(バイナリ) の hex 小文字64桁>`**（コンテンツアドレス、
  計画書 §3.2）。乱数 ID は発行しない。
  - 同一バイナリの再アップロードは**冪等**: 既存レコードを上書きせず同じ ID を
    200 で返す（新規は 201）。
  - ID がコード内容と1対1で結び付くため、アップローダはバイナリを事前に監査し、
    手元で計算したハッシュ（`runner-cli.sh program-id`）をホワイトリストに載せられる。
    ID の発行をサーバに信頼委譲する必要がない。
- 保存は `programs/<id>.bin`（バイナリそのまま）+ `programs/<id>.json`（メタデータ:
  `program_id`/`size`/`uploaded_at`/`uploader`）。blob → meta の順の原子的書き込み
  （データ登録と同じ方式）。
- プログラムはデータ（`/data`）と異なり**状態機械・削除証明の対象ではない**
  （put / 参照 / 削除のみ）。DEK による二重暗号化もせず、encrypted mount の保護
  （層1）にのみ依存する。
- **取得時の整合性検証**: 実行時のロードで blob の sha256 を ID と突き合わせ、
  不一致なら 500（encrypted mount の保護に加えた二重チェック）。
- `DELETE /programs/{id}` は meta → blob の順に除去（meta 除去がコミットポイント。
  orphan blob はロード対象にならず無害）。削除後に同一バイナリを再アップロード
  すれば同じ ID に復元される（ホワイトリスト側の参照は書き換え不要）。
- 一覧 API（`GET /programs`）は当面設けない（計画書 §7-Q5）。

**`POST /execute`（`handleExecute`）**

ボディ＝JSON（`executeRequest`）:

```json
{ "program_id": "p-...", "data": ["d-...", "..."], "args": ["..."] }
```

- `program_id`（必須）: 事前アップロード済みプログラムの ID。欠落・空は 400、
  未知は 404。旧 `wasm` フィールドは未知フィールドとして 400。
- `data`（省略可）: 使用する登録済みデータのIDを 0 個以上・可変長で指定。指定順の
  i 番目が読み取り専用ファイル `/data/input<i>` として WASM から見える。
  空のID・重複は 400。0 個＝ステートレス実行（ライフサイクル管理・削除証明の
  対象外。ただし**オーナー署名は必要**、計画書 §7-Q4）。
- `args`（省略可）: WASI argv。指定順に `argv[1]` 以降として渡り、`argv[0]` は固定値
  `app.wasm`。合計 `maxArgsBytes` 超過は 413。ライフサイクル管理の対象にならない。
- 認可は2段階（計画書 §3.3）: **(1) リクエスト署名がオーナー鍵**（403）、
  **(2) 指定した各データについて `program_id ∈ allowed_programs`**（1件でも外れると
  403。応答のエラーメッセージ先頭に拒否したデータIDが付く）。
- 指定した全データが実行中 `IN_USE` になる。取得は all-or-nothing（§5.2）。
- プログラム本体はレジストリから取得した時点のスナップショットで実行される
  （実行中に `DELETE /programs/{id}` されても当該実行は継続する）。

### 4.4 データ登録とホワイトリスト

**`POST /data`（`handleRegister`）**

- ボディ＝JSON `{"data":"<base64>","allowed_programs":["p-...",...]}`。
  `data` は必須・非空・デコード後 32 MiB 以下。
- **任意の Ed25519 鍵による署名必須**。事前のユーザ登録は不要（公開鍵の提示＋署名
  検証のみの自己認証的な識別、計画書 §3.3）。署名者の公開鍵（base64）が
  `metaRecord.Uploader` に記録され、「誰がアップロードしたか」の識別と、
  ホワイトリスト編集・削除の認可主体になる。
- `allowed_programs`（省略可）: このデータに対して実行を許可する program_id の集合。
  **省略・空配列＝すべて拒否（deny by default、計画書 §7-Q8）**。形式不正
  （`p-<64hex>` 以外）・重複は 400。コンテンツアドレス参照のため**未アップロードの
  プログラムも指定できる**（実行時に未登録なら 404）。
- 削除証明にアップローダ識別子は**含めない**（計画書 §7-Q6。公開鍵がプライバシー上の
  準識別子になることを避ける。削除証明の内容は従来どおり）。

**`PUT /data/{id}/programs`（`handleSetAllowedPrograms` / `setAllowedPrograms`）**

- ボディ＝JSON `{"allowed_programs":[...]}`。**全置換**の意味論（差分 API は無し。
  冪等で単純）。空配列・フィールド省略で「すべて拒否」に戻せる。
- 認可: **当該データに記録済みのアップローダ鍵の署名のみ**（403）。オーナー鍵では
  編集できない（アップローダ専権。オーナーが自分に都合よく許可を広げることを防ぐ）。
- 編集可能なのは削除系状態に入るまで: DELETED は 404、DELETING は 409。
  **IN_USE 中の編集は可能**（レコードロック下で直列化。実行への適用は
  「execute 開始時点のスナップショット」で、実行途中の変更は当該実行に影響しない）。
  IN_USE 中の編集の永続化は、ディスク上では REGISTERED として書き出す
  （「IN_USE は永続化しない」不変条件の維持）。
- ホワイトリストは削除証明の対象外（データ本体のライフサイクルにのみ証明を発行）。

### 4.5 エラー → HTTP ステータス対応

| エラー | execute | data delete | programs put | whitelist edit | status/proof |
|---|---|---|---|---|---|
| オーナー鍵未登録（`errNoOwner`。全コマンドの前段） | 403 | 403 | 403 | 403 | —（対象外） |
| 署名不正・ウィンドウ逸脱・リプレイ（`errUnauthorized`） | 401 | 401 | 401 | 401 | — |
| 鍵はオーナーでない / 認可された鍵でない | 403 | 403 | 403 | 403 | — |
| `errProgramNotFound`（未知の program_id） | 404 | — | 404（delete） | — | — |
| `errNotFound`（未知のデータID） | 404 | 404 | — | 404 | 404 |
| `errDeleted`（削除済み） | **404**（§5 不変条件3） | **409** `already deleted; ...proof` | — | **404** | —（DELETED は正常応答） |
| `errBusy`（IN_USE/DELETING 中） | 409 | 409 | — | 409（DELETING のみ。IN_USE は編集可） | — |
| `errProgramNotAllowed`（ホワイトリスト外） | **403**（対象データIDを応答に含む） | — | — | — | — |
| `errNotDeleted`（未削除） | — | — | — | — | proof のみ 409 |
| その他（I/O・整合性検証失敗等） | 500 | 500 | 500 | 500 | 500 |

## 5. ライフサイクル状態機械（`lifecycle.go`）

状態と遷移は従来どおり（REGISTERED ⇄ IN_USE、REGISTERED → DELETING → DELETED）。
本改修での変更点:

- `metaRecord` から `owner_id` を廃し、**`uploader`**（アップローダ公開鍵の base64）と
  **`allowed_programs`**（program_id の配列。常に非 nil）を持つ（§6.2）。
- `beginExecute(ids, programID)`: 従来の「所有者照合」に代えて、全件検証の中で
  **`programID ∈ allowed_programs` を照合**する（外れは `errProgramNotAllowed`）。
  検証順は 存在 → ホワイトリスト → 状態。all-or-nothing・単一ロック・IN_USE 非永続化
  は従来どおり。
- `delete(id, requester, isOwner)`: `isOwner`（ハンドラがオーナー鍵一致を判定済み）
  または `requester == rec.Uploader` のときのみ許可。
- `setAllowedPrograms(id, requester, programs)`: ホワイトリストの全置換（§4.4）。
  ホワイトリストは状態機械の状態ではなく**可変メタデータ**であり、編集は状態遷移を
  伴わない（既存の原子的メタ書き込みで永続化）。
- 排他制御モデル・クラッシュリカバリ・データID生成・削除（クリプトシュレッディング）
  は従来どおり（単一 mutex、DELETING はリカバリで完遂、tombstone 永続保持）。

## 6. 封印ストレージ（`storage.go`）

### 6.1 二層の暗号化

従来どおり: 層1 = Gramine encrypted mount（`DATA_DIR` 以下全ファイル）、
層2 = データ本体のみ DEK（AES-256-GCM）。**プログラム blob と owner.json は層1のみ**
（削除証明・クリプトシュレッディングの対象でないため。プログラムの完全性は
ロード時のコンテンツハッシュ照合で別途検証する。§4.3）。

### 6.2 ファイル配置とフォーマット

```
DATA_DIR/
  owner.json                 オーナー鍵（ownerRecord: public_key / registered_at）
  meta/<data_id>.json        データメタデータ（metaRecord の JSON）
  blobs/<data_id>.bin        データ本体: nonce(12B) || AES-256-GCM暗号文+認証タグ
  programs/<program_id>.json プログラムメタデータ（programRecord: program_id / size / uploaded_at / uploader）
  programs/<program_id>.bin  WASM バイナリ（そのまま。sha256 が ID と一致することをロード時に検証）
```

`metaRecord`（`lifecycle.go`）の JSON フィールド:

| フィールド | 内容 |
|---|---|
| `data_id` | データID（`d-` + 8バイト乱数 hex） |
| `state` | `REGISTERED` / `IN_USE` / `DELETING` / `DELETED` |
| `content_hash` | `sha256:<64hex>`（登録時点の元データ（平文）のハッシュ。削除証明と照合） |
| `created_at` | RFC3339 UTC |
| `uploader` | アップローダの Ed25519 公開鍵（raw 32B の base64）。ホワイトリスト編集・削除の認可主体 |
| `allowed_programs` | 実行を許可する program_id の配列（空＝すべて拒否） |
| `dek` | DEK の base64。**削除時に除去される** |
| `deleted_at` | RFC3339 UTC（削除後のみ） |
| `certificate` | 発行済み削除証明の JSON（削除後のみ、埋め込み） |

書き込みの原子性・順序（`atomicWrite`、blob → meta）と削除（DELETING 永続化 =
クリプトシュレッディングのコミットポイント）は従来どおり。

## 7. WASM 実行サンドボックス（`sandbox.go`）

変更なし（ネットワーク禁止・読み取り専用メモリFS・出力上限・タイムアウト・
メモリ上限・最小権限）。実行対象バイナリの供給元が「リクエストボディ直載せ」から
「プログラムレジストリ」に変わっただけで、`run` のインタフェース・制約は同一。

## 8. 削除証明（`attestation.go`）

変更なし（起動時の Ed25519 鍵生成 + user_report_data への公開鍵ハッシュ埋め込み、
`certificateCore` への署名、quote/ローカルレポートの縮退動作）。
削除証明にアップローダ識別子は含めない（計画書 §7-Q6）。
なお、オーナー鍵・アップローダ鍵（ユーザ側の Ed25519 鍵）と削除証明の署名鍵
（エンクレーブ内で生成）は無関係である。

## 9. Gramine 統合（リポジトリ直下）

- マニフェスト・Makefile は本改修で**変更不要**（計画書 T8: 案B のため
  `OWNER_PUBKEY` 等の追加なし）。`owner.json`・`programs/` は `DATA_DIR`
  （`/data_store` encrypted mount）配下に置かれるため、meta/blobs と同様に
  層1の保護下に入る。
- 運用上の注意（従来どおり + 追加）:
  - `SGX`/`RA_TYPE` 切り替え時は `make clean` とマニフェスト再生成。
  - SGX ↔ direct を跨いで同じ `data_store/` は読めない（封印鍵のドメインが異なる）。
    切り替え時は `data_store/` を空にし、**オーナー登録からやり直す**。
  - デプロイ手順: 起動 → 即 `runner-cli.sh owner-register --wait`（§4.1 TOFU の
    先取りリスク対策として、この2つは一連の手順として行うこと）。

## 10. 既存機能からの変更点（2026-07-10、計画書 U1–U5）

- **API キー方式の全廃**: `POST /users`・`users.json`・`X-API-Key` /
  `Authorization: Bearer` を廃止（併存期間なしの一括置き換え、計画書 §7-Q7）。
  認証はすべて Ed25519 署名（§4.1）。乱数 owner_id（`u-...`）も廃止し、主体の
  識別子は公開鍵そのものになった。
- **オーナーモデルの導入**: サーバは「1オーナーのランナー」となり、デプロイ時に
  TOFU で公開鍵を登録する（`POST /owner`）。execute・プログラム管理はオーナー専権。
- **WASM の事前アップロード**: `POST /execute` の `wasm`（base64 直載せ）を廃止し、
  `POST /programs` で事前登録した `program_id`（`p-<sha256>`）を参照する方式に変更。
  execute ボディ上限は 44 MiB → 1 MiB に縮小。
- **アップローダ識別とホワイトリスト**: `POST /data` はボディが生データ →
  JSON（base64）になり、任意鍵の署名が必須になった。データごとに
  `allowed_programs`（deny by default）を持ち、編集はアップローダ専権
  （`PUT /data/{id}/programs`）。execute の認可は「所有者照合」から
  「オーナー署名 + データごとのホワイトリスト照合」の2段階に再定義された。
- **旧 `data_store/` とは非互換**: `users.json` は不要になり、`metaRecord` の
  `owner_id` が `uploader`（公開鍵）に置き換わり、`allowed_programs` が加わった。
  移行ツールは作らない（`data_store/` を空にして登録し直す。計画書 §6）。
- 旧 API の応答: `POST /users` は 404。execute の `wasm` フィールドは 400
  （未知フィールド拒否）。`POST /data` への生データボディは 400（JSON でないため）。

## 11. 制限事項・設計判断

- **TOFU の先取りリスク**: `POST /owner` は認証不要のため、デプロイから登録までの
  間に攻撃者が先に鍵を登録できてしまう。対策は運用（起動→即登録を一連で行う）のみ
  （計画書 §7-Q1 で受容済み。案A の環境変数方式なら排除できるが再デプロイが必要に
  なるため不採用）。先取りされた場合はコマンドが全部 403/409 になるので即座に
  検知でき、`data_store/` を消して再デプロイすればよい（データ登録前なら実害なし）。
- **時計の信頼**: タイムスタンプ検証に使う時計はホスト由来（TEE 内でも信頼できない）。
  ホストが時計を操作すればリプレイウィンドウを広げられる。既知の制約として受容し、
  必要になれば nonce（チャレンジ・レスポンス）方式へ移行する（計画書 §7-Q2）。
- **リプレイキャッシュはメモリ上のみ**: 再起動直後はキャッシュが空になるため、
  ウィンドウ内の署名は再起動を跨いで一度だけ再利用され得る（ホストがプロセスを
  再起動させられる前提では、ウィンドウ幅ぶんのリプレイ猶予と等価）。副作用が
  冪等でないコマンドは execute のみで、影響は限定的と判断。
- **メタデータのロールバック**: Gramine Protected Files はロールバック
  （削除前の meta のバックアップを書き戻す攻撃）を防げない。`owner.json` も同様で、
  ホストが「owner.json が無い状態」に巻き戻せば TOFU の再登録窓が開く。
  モノトニックカウンタ等による対策は未実装（従来からの未解決課題を継承）。
- **出力の開示範囲（信頼モデル）**: アップローダが許可したプログラムの出力は
  オーナーに渡る。アップローダは「出力が開示されてよいプログラムだけを許可する」
  責任を負う（生データをそのまま出力するプログラムを許可すれば、実質的に生データを
  オーナーに開示したことになる。計画書 §3.3）。program_id はバイナリの sha256 な
  ので、許可は「特定のコード内容の承認」を意味し、アテステーションと組み合わせる
  ことで「自分が監査・承認したコードだけが自分のデータを読める」ことをエンド
  ツーエンドで保証できる。
- **ホワイトリストの読み出し API は無い**: `status` は従来フィールドのまま
  （uploader 公開鍵・ホワイトリストを無認証で晒さない）。PUT は全置換なので
  読み出しなしで安全に設定できる。
- **DEK のメモリゼロ化は best-effort**・**`deleted_at` はリカバリ時刻になり得る**・
  **ボディ上限は定数**: 従来どおり。

## 12. テスト（`*_test.go`）

実行方法（ホストに Go 不要）:

```sh
cd wasm_runner
docker run --rm -v "$PWD":/work -w /work golang:1.25-bookworm go test ./... -count=1
# race detector 付き:
docker run --rm -v "$PWD":/work -w /work golang:1.25-bookworm go test ./... -count=1 -race
```

| テスト | 検証内容 |
|---|---|
| `TestSignedMessageFormat` | 署名対象メッセージの正規化形式（空ボディ含む） |
| `TestAuthenticateAcceptsValidSignature` / `TestAuthenticateRejectsInvalidRequests` | 署名検証の肯定系と、ヘッダ欠落・不正鍵・鍵不一致・ボディ/メソッド/パス改ざん・不正タイムスタンプの否定系 |
| `TestAuthenticateTimestampWindow` | 許容ウィンドウ内（過去・未来）の受理と逸脱の拒否 |
| `TestAuthenticateRejectsReplay` | 同一署名の再提示の拒否と再署名での受理 |
| `TestOwnerRegisterTOFU` / `TestOwnerPersistenceAcrossRestart` | TOFU（不正鍵の拒否・初回のみ受理・二重登録拒否）と再起動後の保持 |
| `TestProgramIDFormat` / `TestProgramPutGetDelete` / `TestProgramPersistenceAcrossRestart` / `TestProgramIntegrityCheck` | コンテンツアドレス ID・冪等 put・削除と同一 ID への復元・再起動後の保持・blob 改ざん検知 |
| `TestRegisterAndStatus` | 登録（uploader/whitelist の記録、deny by default）、未知IDは errNotFound |
| `TestExecuteLifecycle` / `TestExecuteMultiData` | REGISTERED⇄IN_USE 遷移、IN_USE 中の排他、複数データの all-or-nothing（ホワイトリスト外・削除済み混在を含む） |
| `TestWhitelistEnforcement` / `TestSetAllowedPrograms` | ホワイトリスト照合（拒否時の状態不変）と編集（アップローダ専権・IN_USE 中の編集と REGISTERED での永続化・全置換・再起動後の保持・削除済みは編集不可） |
| `TestDeleteAuthorization` | 削除の認可（無関係鍵の拒否、アップローダ・オーナーの許可） |
| `TestDeleteIssuesCertificateAndBecomesTerminal` | 証明フィールドの整合、アップローダ識別子の非含有（§7-Q6）、DELETED の終端性 |
| `TestConcurrentDeleteRace` / `TestPersistenceAcrossRestart` / `TestCryptoShreddingOnDisk` / `TestCrashDuringDeleteRecovery` / `TestSealOpenRoundtrip` | 従来どおり（削除競合・永続化・クリプトシュレッディング・リカバリ・暗号往復） |
| `TestAPILifecycleFlow` | HTTP 一巡（owner 登録 → program 登録 → data 登録 → execute → uploader による delete → proof）+ 証明の署名検証 |
| `TestAPIOwnerTOFU` | 未登録時の全コマンド 403（署名あり/なし両方）・認証不要 API の応答・不正鍵 400・初回 201・二重登録 409・登録後の疎通 |
| `TestAPIAuth` | ヘッダ無し 401・ボディ改ざん 401・別パス署名の流用 401・期限切れ 401・リプレイ 401・非オーナー鍵のオーナー専用コマンド 403 |
| `TestAPIProgramRegistry` | 登録 201（ID がローカル計算と一致）・冪等 200・空 400・削除 200 → execute 404 → 再登録 201 |
| `TestAPIExecuteValidation` | 非JSON 400・旧 `wasm` フィールド 400・program_id 欠落 400・未知 program 404・データID検証 400・不正 WASM の実行エラー 400 |
| `TestAPIExecuteMultiData` | 複数データ実行（`input0`/`input1` の内容と順序）・ホワイトリスト外混在 403（対象ID入り・状態不変）・未知ID混在 404 |
| `TestAPIWhitelistEdit` | deny by default・オーナー/第三者鍵の編集 403・アップローダの全置換と空置換・形式/重複 400・未知/削除済み 404 |
| `TestAPIDeleteAuthorization` | 無関係鍵 403・オーナー削除・アップローダ削除（証明検証付き） |
| `TestAPIRegisterValidation` / `TestAPIBodyTooLarge` | ボディ検証（非JSON・空・不正 base64・旧フィールド・不正/重複 program_id）と 413 |
| `TestAPIStatelessExecute` / `TestAPIExecuteArgs` / `TestAPIHealth` / `TestAPIExecuteBusyConflict` | ステートレス実行のオーナー署名必須（401/403/200）・args の受け渡し・ヘルスチェック・IN_USE 競合 409 |
| `TestSandbox*` / `TestCappedBuffer` | 従来どおり（タイムアウト・メモリ上限・入力マウント・argv・出力上限） |

`wasm_test.go` はテスト用の最小 WASM バイナリ（no-op / 無限ループ / 大メモリ要求 /
argv エコー）を手組みで生成するヘルパ。
