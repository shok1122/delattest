# wasm_runner 実装仕様書（as-built）

本書は `wasm_runner` に実装したデータライフサイクル管理機能の**実装仕様**である。
設計仕様書 `docs/data-lifecycle-spec.md`（以下「設計書」、§n は設計書の章番号）に
対して、実際のコードが何をどう実装しているかを記述する。実装レビュー時は本書と
コードを突き合わせて確認できるよう、各節に実装箇所（ファイル・関数名）を記す。

検証状況: ユニットテスト28件 + race detector パス。gramine-direct / gramine-sgx
（`RA_TYPE=none`）で全APIの実機確認済み（2026-07-08、`POST /execute` への統合・
複数データ指定は 2026-07-09 に非 Gramine のローカル実行で全ケース確認済み）。
ユーザ認証（`POST /users` / APIキー / owner_id 照合、2026-07-09 導入）は
非 Gramine のローカル実行で一巡確認済み（Gramine 実機は未確認）。

## 1. ファイル構成と責務

| ファイル | 責務 | 設計書対応 |
|---|---|---|
| `main.go` | 起動処理（設定読み込み → store → prover → lifecycleManager → HTTPサーバ）、graceful shutdown | — |
| `lifecycle.go` | 状態機械・メタデータ管理・排他制御・クラッシュリカバリ（Lifecycle Manager） | §5, §6.1 |
| `users.go` | ユーザ表（owner_id ↔ APIキーハッシュ）の管理・認証（User Manager） | §7.1 |
| `storage.go` | 封印ストレージのファイル配置・AES-256-GCM 暗号化・原子的書き込み | §6.2, §9.1 |
| `sandbox.go` | WASM 実行サンドボックス（wazero、実行制約） | §6.3, §8 |
| `attestation.go` | 署名鍵の生成・SGX quote/レポート取得・削除証明の発行 | §6.4, §9 |
| `handlers.go` | HTTP API ルーティング・リクエスト検証・エラー応答 | §7 |
| `*_test.go` | ユニットテスト（§12 参照） | — |
| `testdata/readinput.wasm` | `/data/input0`, `/data/input1`, ... を順に連結出力するテスト用フィクスチャ（`wasm_module/readinput/` のビルド成果物） | — |

## 2. 起動シーケンス（`main.go: main`）

1. `HOST`/`PORT` から待ち受けアドレスを決定。
2. `newStore(DATA_DIR)` — `meta/`・`blobs/` サブディレクトリを作成（0700）。
3. `newUserManager(store)` — 永続化済みのユーザ表（`users.json`、§4.1）をロード。
4. `newProver()` — Ed25519 鍵ペアを生成し、`/dev/attestation/*` から quote または
   ローカルレポートを取得（取得できない環境では署名のみのモードに縮退。§10）。
5. `newLifecycleManager(store, prover)` — 永続化済みメタデータを全件ロードし、
   中断状態をリカバリ（§6.4）。リカバリ失敗時は起動エラーで停止。
6. サンドボックス設定（`EXEC_TIMEOUT_SEC`, `WASM_MEM_LIMIT_PAGES`）を読み込み、
   HTTP サーバを起動。SIGINT/SIGTERM で 5 秒タイムアウトの graceful shutdown。

起動ログ例（SGX, RA無効時）:

```
SGX local report acquired (mrenclave=8bcc...); no quote (remote attestation disabled)
listening on http://0.0.0.0:3000 (attestation: none, data dir: /data_store)
```

## 3. 設定

### 環境変数（`main.go: getEnv/envInt`）

| 変数 | 既定値 | 説明 |
|---|---|---|
| `HOST` / `PORT` | `0.0.0.0` / `3000` | 待ち受けアドレス |
| `DATA_DIR` | `data_store` | 封印ストレージの場所。Gramine 実行時はマニフェストの `loader.env.DATA_DIR = "/data_store"`（encrypted mount）が優先される |
| `EXEC_TIMEOUT_SEC` | `30` | WASM 実行タイムアウト（秒、正の整数を想定） |
| `WASM_MEM_LIMIT_PAGES` | `1024` | WASM メモリ上限（64 KiB ページ数）。1〜65536 の範囲外は起動時エラー |

### 定数

| 定数 | 値 | 場所 |
|---|---|---|
| `maxDataBytes` | 32 MiB | `handlers.go`。`POST /data` のボディ上限 |
| `maxWasmBytes` | 32 MiB | `handlers.go`。WASM バイナリの上限 |
| `maxArgsBytes` | 8 KiB | `handlers.go`。`POST /execute` の引数（`?arg=`）の合計上限 |
| `outputCap` | 1 MiB | `sandbox.go`。stdout / stderr それぞれの上限（超過分は切り捨て） |

リクエストボディは全量をメモリに読み込む（最大 32 MiB）。エンクレーブサイズ
（現状 1G）とのバランスで決めた値。

## 4. HTTP API 詳細（`handlers.go`）

ルーティングは Go 1.22+ の `http.ServeMux` パターン（`DELETE /data/{id}` 等）。
未定義パスは 404（`404 page not found`）、メソッド不一致は 405 を返す。

エラー応答はすべて JSON `{"error":"<メッセージ>"}`。text/plain を返すのは
`GET /`（ヘルスチェック）と実行結果（`POST /execute` 成功時）のみ。

### 4.1 認証と認可（`users.go` / `apiKey` / `checkOwner`）

認証（誰であるか＝APIキーの検証）と認可（そのデータを操作できるか＝owner_id の
照合）を分離している（設計書 §7.1）:

- **ユーザ発行**: `POST /users` が owner_id（`u-` + 8バイト乱数 hex）と APIキー
  （`ak-` + 32バイト乱数 hex）の対を発行する。サーバは sha256(APIキー) の hex だけを
  ユーザ表（`DATA_DIR/users.json`）に保存し、キーの平文は発行時の応答限り。
- **認証**: APIキーは `X-API-Key: <key>` または `Authorization: Bearer <key>` で渡す
  （`X-API-Key` 優先）。`userManager.resolveOwner` が sha256(キー) でユーザ表を引いて
  owner_id に解決する。未提示・無効なキーは 401。
- **認可**: データレコードには登録者の owner_id が保存され（`metaRecord.OwnerID`）、
  `execute`/`delete` 時に `checkOwner` が認証済みユーザとレコードの owner_id を
  照合する。不一致は 403。「利用者＝データオーナー本人」の前提（設計書 §3）は
  この照合によってシステム的に保証される。
- 認証必須の操作: `POST /data`・データ指定のある `POST /execute`・`DELETE /data/{id}`。
  ステートレス実行（data 指定なし）と `status`/`proof` は認証不要
  （生データ・鍵を含まず、proof は監査用のため）。
- owner_id は秘密ではない識別子であり、ログ・監査記録に含めてよい。秘密はAPIキー
  のみで、漏洩時はユーザ表のエントリ差し替えで失効できる（ローテーション・失効APIと
  `POST /users` 自体の保護は未実装。設計書 §11）。

### 4.2 エンドポイント一覧

| メソッド / パス | 成功応答 | エラー |
|---|---|---|
| `GET /` | 200 text（エンドポイント一覧） | — |
| `POST /users` | **201** JSON `{"owner_id","api_key","created_at"}` | 500 |
| `POST /execute` | 200 text（実行結果） | 400 / 401 / 403 / 404 / 409 / 413 / 500 |
| `POST /data` | **201** JSON `{"data_id","registered_at"}` | 400（空ボディ）/ 401 / 413（32MiB超）/ 500 |
| `DELETE /data/{id}` | 200 JSON（削除証明） | 401 / 403 / 404 / 409 / 500 |
| `GET /data/{id}/status` | 200 JSON | 404 |
| `GET /data/{id}/proof` | 200 JSON（削除証明） | 404 / 409 |

### 4.3 WASM 実行とデータ指定（`POST /execute`）

- ボディ＝WASM バイナリ。クエリパラメータ `data` の繰り返しで、使用する登録済み
  データを**0個以上・可変長**で指定する（`POST /execute?data=<id1>&data=<id2>`）。
- 指定順の i 番目のデータが、読み取り専用ファイル `/data/input<i>`（`input0` 起点）
  として WASM から見える（§7.1）。
- `data` 指定なし＝ステートレス実行（旧 `POST /execute-wasm` 相当）。ライフサイクル
  管理・削除証明の対象にならず、FS 自体をマウントしない。
- 空のID・同一IDの重複指定は 400（`beginExecute` に渡る前にハンドラで拒否）。
- クエリパラメータ `arg` の繰り返しで **WASI argv** を0個以上指定できる
  （`POST /execute?data=<id>&arg=get&arg=github`）。指定順に `argv[1]` 以降として
  渡り、`argv[0]` は固定値 `app.wasm`（`arg` 指定が1個も無ければ argv 自体を
  提供しない）。`arg` は使い捨ての実行パラメータであり、ライフサイクル管理
  （登録・削除証明）の対象にならない。空文字列の `arg` はそのまま argv として
  渡す（`data` と違い 400 にしない）。合計 `maxArgsBytes` 超過は 413。
  認証要件は `data` 側で決まる（`arg` のみの指定では認証不要）。
  注意: クエリ文字列はプロキシ・アクセスログ等に残り得るため、秘密値
  （パスワード等）を `arg` で渡す場合はログ経路に留意すること。
- 指定した**全データ**が実行中 `IN_USE` になる。取得は all-or-nothing（§5.2）で、
  1件でも検証に失敗するとどのデータの状態も変更せず、エラーメッセージの先頭に
  対象のデータIDを付けて返す（例: `{"error":"d-...: data not found"}`）。
- データを1個以上指定する場合は認証必須（§4.1）で、指定した**全データ**が
  認証済みユーザの所有でなければならない。他ユーザのデータが1件でも混ざれば
  403 で、どのデータの状態も変更されない（all-or-nothing）。

### 4.4 ライフサイクルエラー → HTTP ステータス対応

`lifecycle.go` のエラー種別ごとに、各ハンドラで次のように対応付ける
（execute のエラーメッセージには対象のデータIDが前置される）:

| エラー | execute | delete | status | proof |
|---|---|---|---|---|
| `errNotFound`（未知のID） | 404 `data not found` | 404 | 404 | 404 |
| `errDeleted`（削除済み） | **404** `data already deleted`（§5 不変条件3） | **409** `already deleted; deletion certificate is available at GET /data/{id}/proof` | —（DELETED は正常応答） | —（DELETED は正常応答） |
| `errBusy`（IN_USE/DELETING 中） | 409 | 409 | — | — |
| `errNotDeleted`（未削除） | — | — | — | 409 `data is not deleted yet` |
| `errForbidden`（所有者不一致） | 403 | 403 | — | — |
| `errUnauthorized`（APIキー未提示・無効。ハンドラで先に判定） | 401 | 401 | — | — |
| その他（I/O等の内部エラー） | 500 | 500 | — | 500 |

補足: `execute` の 404 は「未知のID」と「削除済み」でメッセージが異なる
（削除済みIDの存在自体は proof が取得できる以上秘匿対象ではない、という判断）。

### 4.5 応答例

```
POST /data                       → 201 {"data_id":"d-f00ff651ebde5e56","registered_at":"2026-07-08T08:24:42Z"}
GET  /data/{id}/status           → 200 {"data_id":"d-...","state":"REGISTERED","registered_at":"..."}
GET  /data/{id}/status （削除後）→ 200 {"data_id":"d-...","state":"DELETED","registered_at":"...","deleted_at":"..."}
```

実行結果（`sandbox.go: run` の整形）:

- stderr が空 → stdout の内容をそのまま返す
- stderr が非空 → `-- stdout --\n<stdout>\n\n-- stderr --\n<stderr>` の形式

## 5. ライフサイクル状態機械（`lifecycle.go`）

### 5.1 状態と遷移

```
              register
(nonexistent) ────────> REGISTERED ⇄ IN_USE     （⇄ = beginExecute / endExecute）
                            │
                            │ delete
                            V
                        DELETING ────> DELETED（終端）
```

| 遷移 | 実装 | 永続化 |
|---|---|---|
| → REGISTERED | `register` | する（blob → meta の順） |
| REGISTERED → IN_USE | `beginExecute`（指定された全データを一括遷移） | **しない**（メモリ上のみ。クラッシュ時はディスク上 REGISTERED のまま復帰） |
| IN_USE → REGISTERED | `endExecute`（ハンドラの defer で必ず呼ばれる、全データ一括） | しない |
| REGISTERED → DELETING | `delete` 前半 | **する（DEK を落とした状態で）** — クリプトシュレッディングのコミットポイント |
| DELETING → DELETED | `finishDelete`（blob消去 → 証明発行 → 永続化） | する（証明込み） |

### 5.2 排他制御モデル

- 単一の `sync.Mutex`（`lifecycleManager.mu`）が**全レコードの状態遷移を直列化**する
  （§5 不変条件4「状態を書き換えられる経路の一本化」）。
- WASM 実行そのもの（長時間処理）はロックの外で行い、その間は `state = IN_USE` が
  データID単位の排他を担う（§5 不変条件2, TOCTOU 対策）。
- `beginExecute` は複数データを受け取り、**全件の検証（存在・所有者・状態）→
  全件の復号 → 全件の IN_USE 遷移**を単一のロック区間内で行う。途中で失敗した
  場合は一切状態を変更しない（all-or-nothing）ため、部分的に IN_USE のまま残る
  データは生じない。ロックが単一なので、複数データの同時取得によるデッドロックも
  構造上起こらない。使用中のデータが1件でも重なる別の execute/delete は 409 になる。
- 帰結として、`delete` はロックを保持したまま同期的に完了するため、API 経由で
  `DELETING` 状態が観測されることは実質ない（`DELETING` はディスク上の中間状態）。
- トレードオフ: register/beginExecute のファイルI/O（≤32MiB の暗号化/復号）も
  ロック内で行うため、異なるID間でも直列化される。正しさ優先の設計。

### 5.3 データID（`newDataID`）

- `d-` + 8バイト乱数の hex（計 16 hex 文字）。`crypto/rand` 使用。
- DELETED のレコード（tombstone）も `entries` マップに永続的に残るため、
  削除済みIDとの衝突＝再登録は起こらない（§5 不変条件3）。衝突時は再生成（最大10回）。

### 5.4 クラッシュリカバリ（`newLifecycleManager`）

起動時に `meta/*.json` を全件ロードし、状態ごとに:

| ディスク上の状態 | 処理 |
|---|---|
| `REGISTERED` / `DELETED` | そのままロード |
| `IN_USE` | `REGISTERED` に戻して永続化（通常は永続化されない状態のため防御的措置） |
| `DELETING` | `finishDelete` を実行して削除を完遂（blob消去 → 証明発行 → DELETED）。このとき `deleted_at` はリカバリ時刻になる |

リカバリ中のエラーは起動失敗として扱う（不整合状態でのサービス開始を防ぐ）。

### 5.5 不変条件の実装対応（設計書 §5）

1. **生値を返す API が無い**: `GET /data/{id}` は定義していない。`status` 応答は
   `statusInfo` 構造体（data_id / state / registered_at / deleted_at のみ）。
2. **IN_USE 中の排他**: §5.2 のとおり。
3. **DELETED 終端・ID再利用不可**: execute は 404、tombstone 永続保持。
4. **管理経路の一本化**: 状態変更はすべて `lifecycleManager` のメソッド経由。

## 6. 封印ストレージ（`storage.go`）

### 6.1 二層の暗号化

| 層 | 主体 | 鍵 | 目的 |
|---|---|---|---|
| 1 | Gramine encrypted mount（Protected Files） | SGX: `_sgx_mrsigner` シーリング鍵 / direct: 開発用固定鍵 | `DATA_DIR` 以下**全ファイル**（meta含む）をホストから秘匿。ホスト上は `GRAFS_PF` マジックの暗号化ファイルにしかならない |
| 2 | アプリ（本実装） | データごとの DEK（32バイト乱数、AES-256-GCM） | データ**本体**の暗号化。削除時に DEK を破棄することでクリプトシュレッディングを成立させる（§9.1） |

### 6.2 ファイル配置とフォーマット

```
DATA_DIR/
  users.json             ユーザ表（userRecord の JSON 配列: owner_id / sha256(APIキー) / created_at）
  meta/<data_id>.json    メタデータ（metaRecord の JSON）
  blobs/<data_id>.bin    データ本体: nonce(12B) || AES-256-GCM暗号文+認証タグ
```

`metaRecord`（`lifecycle.go`）の JSON フィールド:

| フィールド | 内容 |
|---|---|
| `data_id` | データID |
| `state` | `REGISTERED` / `IN_USE` / `DELETING` / `DELETED` |
| `content_hash` | `sha256:<64hex>`（**登録時点の元データ（平文）**のハッシュ。削除証明と照合するためのもの） |
| `created_at` | RFC3339 UTC |
| `owner_id` | 登録者の owner_id（§4.1。秘密ではない識別子） |
| `dek` | DEK の base64（Go の `[]byte` JSON 表現）。**削除時に除去される** |
| `deleted_at` | RFC3339 UTC（削除後のみ） |
| `certificate` | 発行済み削除証明の JSON（削除後のみ、埋め込み） |

### 6.3 書き込みの原子性・順序

- すべての書き込みは `atomicWrite`（`.tmp` に書いて `rename`）。ロード時に
  拡張子 `.json` 以外を無視するため、書きかけの `.tmp` が残っても影響しない。
- `register` は **blob → meta の順**。blob 書き込み後・meta 書き込み前にクラッシュ
  しても、meta の無い blob はロード対象にならず登録は不成立（orphan blob は
  対応する DEK も存在しないため無害）。

### 6.4 削除とクリプトシュレッディング（`delete` / `finishDelete`）

```
delete:
  1. （ロック内）検証: 存在・所有者・状態 = REGISTERED
  2. state = DELETING にし、DEK をレコードから外して永続化
       └ 失敗時は state/DEK を元に戻して 500（削除は不成立）
       └ 成功 = コミットポイント。以後どの時点でクラッシュしても
         復号鍵が復元される経路は存在しない。メモリ上の DEK はゼロ化（wipe）
  3. finishDelete:
       a. blobs/<id>.bin を削除（既に無い場合は許容）
       b. deleted_at = 現在時刻、prover.issueCertificate で証明発行
       c. state = DELETED + 証明を meta に永続化
```

暗号化 blob やそのバックアップがホスト側に残存しても、DEK が存在しないため
復号不能＝実質削除（§9.1 の方針どおり）。

## 7. WASM 実行サンドボックス（`sandbox.go`）

### 7.1 実行制約（設計書 §8 との対応）

| 制約 | 実装 |
|---|---|
| ネットワーク禁止（§8-1） | `wasi_snapshot_preview1.Instantiate` による WASI Core Module のみ提供。ソケット系ホスト関数・wazero の experimental sock 設定は使用しない。**将来もネットワーク系ホスト関数を追加しないことが設計原則** |
| FS 制限（§8-2） | 実行時に指定された登録データ（0個以上）を `fstest.MapFS`（**メモリ上の読み取り専用FS**）として guest の `/data` にマウント。WASM からは指定順に `/data/input0`, `/data/input1`, ... だけが見える。書き込みマウント無し。データ指定なし（ステートレス実行）では FS 自体を与えない。**平文がホストのディスクに書かれることはない** |
| 出力上限（§8-3） | stdout/stderr 各 1 MiB（`cappedBuffer`）。DoS対策であり機密性目的ではない。上限到達後の書き込みは「成功」として扱い（モジュールを止めない）、超過分は捨てる。上限をまたぐ書き込みは受理できたバイト数を返す |
| タイムアウト（§8-4） | `context.WithTimeout`（`EXEC_TIMEOUT_SEC`）+ `WithCloseOnContextDone(true)`。無限ループ等の実行中コードも強制中断され、`400 WASM error: execution aborted: context deadline exceeded (limit 30s)` を返す |
| メモリ上限（§8-4） | `WithMemoryLimitPages`（既定 1024 ページ = 64 MiB）。超過を要求するモジュールはコンパイル/インスタンス化で拒否 |
| 最小権限（§8-5） | 環境変数は不提供。引数（WASI argv）は `?arg=` で明示指定された場合のみ提供（§4.3）— リクエスト元が自ら渡す実行パラメータであり、時計・乱数のようなホスト資源ではないため最小権限には抵触しない。時計・乱数は wazero 既定の**決定的な擬似値**のまま（実時間・実乱数を与えない） |

### 7.2 実行フロー（`run`）

1. リクエスト ctx にタイムアウトを重ねる（クライアント切断でも中断される）。
2. **リクエストごとに新しい wazero ランタイム**を生成（実行間の完全分離）。
3. WASI instantiate → `CompileModule` → `InstantiateModule`（`_start` 自動実行）。
4. 終了判定: `proc_exit(0)` による `sys.ExitError`（exit code 0）は正常終了として
   扱う。エラー時に ctx が期限切れならタイムアウトとして報告。
5. 出力整形は §4.5 のとおり。

## 8. 削除証明（`attestation.go`）

### 8.1 起動時の鍵生成とアテステーション（`newProver`）

1. Ed25519 鍵ペアを生成（**メモリ上のみ、プロセス毎に新規**。エンクレーブ外に出ない）。
2. `/dev/attestation/attestation_type` を読む（無ければ `"none"`）。
3. `user_report_data`（64バイト）= `sha256(公開鍵)`（32B）+ ゼロ埋め（32B）を書き込む。
   → 以後取得する quote/レポートに公開鍵ハッシュが焼き込まれ、「真正なエンクレーブが
   この署名鍵を保持している」ことの証明になる（§9.3 手順2 の「起動時に一度だけ」方式）。
4. ローカルレポート用に `my_target_info` → `target_info` を設定。
5. quote 取得を試み、失敗したらローカルレポートに縮退:

| 実行環境 | quote | mrenclave / mrsigner | attestation_type |
|---|---|---|---|
| gramine-sgx + `RA_TYPE=dcap`（ホストにDCAP基盤） | あり（base64） | quote から抽出 | `dcap` |
| gramine-sgx + `RA_TYPE=none` | **空** | ローカルレポートから抽出 | `none` |
| gramine-direct / 素のプロセス | 空 | 空 | `none` |

バイナリ解析のオフセット: quote は 48B ヘッダ + `sgx_report_body_t`、レポートは
body 直開始。body 内 offset 64 が MRENCLAVE、128 が MRSIGNER（各32B）。
quote は 432B 以上、レポートは 384B 以上を要求。

### 8.2 証明フォーマット（`deletionCertificate`）

```json
{
  "data_id": "d-0eb8969f1c310a74",
  "deleted_at": "2026-07-08T08:31:05Z",
  "content_hash": "sha256:d707f3f35941cbe9dd61c0070d9c2aaa091a00c991ea102dd15d77c9e462585d",
  "enclave_report": {
    "mrenclave": "8bcc630e42a74e9117b14dec648aae8a80071933ff95b580c537dd37fc35159e",
    "mrsigner": "d676e128e8170223b9e8e0d2bf4b982a39a815f07cda060117ee97e7d16a9ebe",
    "quote": "",
    "attestation_type": "none",
    "public_key": "viZQ6mhKA5GyXvvMgjDo+juz7L6igSLQZtcmDwGw/U0=",
    "signature_scheme": "ed25519"
  },
  "signature": "WgAlrZ0uMM0LPrIWAr8cCiCNJS4X2XA8rlnWWGts6HuQueoUPQ6zXOdMGLc3XzHXmF5g0VzjOzQ7AGVIZqfYCA=="
}
```

設計書 §9.2 のフォーマットに対し、検証に必要な `public_key`（Ed25519 raw 32B の
base64）・`attestation_type`・`signature_scheme` を `enclave_report` に追加している。
`quote`/`signature` の値は素の base64（設計書例の `base64:` プレフィックスは表記）。

### 8.3 署名対象（`certificateCore`）

署名メッセージは**次の3フィールドをこの順に並べた JSON バイト列**（Go の struct
マーシャル出力。検証者が決定的に再構築できる）:

```json
{"data_id":"d-...","deleted_at":"2026-07-08T08:31:05Z","content_hash":"sha256:..."}
```

これに対する Ed25519 署名が `signature`。

### 8.4 検証手順（第三者検証者、設計書 §9.3 対応）

1. `quote` を DCAP で検証し、`mrenclave`/`mrsigner` が期待値と一致することを確認。
2. quote 内 `user_report_data` の先頭32B が `sha256(base64decode(public_key))` と
   一致することを確認（鍵とエンクレーブの紐付け）。
3. §8.3 のペイロードを再構築し、`signature` を `public_key` で検証。
4. `content_hash` が登録データの sha256 と一致することを確認。

`quote` が空の証明（`RA_TYPE=none`・direct）は手順 1–2 が実施できず、
**署名のみの証明（開発用）**となる。

### 8.5 証明の永続性と鍵のライフタイム

- 証明は削除時に生成され meta に**永続化**される。`proof` は保存済みのものを
  そのまま返すため、再起動後も同一の証明が得られる。
- 署名鍵はプロセス毎に新規生成されるため、プロセスが変われば以後の証明は別の鍵で
  署名される。各証明は公開鍵・quote を自己完結的に含むため、検証可能性は保たれる。

## 9. Gramine 統合（リポジトリ直下）

- `wasm-runner.manifest.template`:
  - encrypted mount: `{ path = "/data_store", uri = "file:data_store", type = "encrypted", key_name = ... }`
    — key は SGX ビルド時 `_sgx_mrsigner`（同一署名者ならバイナリ更新後も開ける）、
    非 SGX 時は `fs.insecure__keys.insecure_dev_key`（保護効果なし、開発用）。
    Jinja の `{% if not sgx %}` で切り替え。
  - `loader.env.DATA_DIR = "/data_store"`。
  - `sgx.remote_attestation = "{{ ra_type }}"`（Makefile の `RA_TYPE`、既定 `none`）。
- `Makefile`: `gramine-manifest` に `-Dsgx=$(SGX) -Dra_type=$(RA_TYPE)` を追加。
  `make run` が `data_store/` を自動作成。
- 運用上の注意:
  - `SGX`/`RA_TYPE` を切り替えたら `make clean` してマニフェスト再生成。
  - **封印鍵のドメインが違うため、SGX ↔ direct を跨いで同じ `data_store/` は
    読めない**。切り替え時は `data_store/` を空にする。
  - `RA_TYPE=dcap` はホストの DCAP 基盤（aesmd + quote provider）が必要。
    未整備だとエンクレーブ起動自体が失敗する（AESM error 12）。

## 10. 既存機能からの変更点

- 認可を「登録時トークンの照合」から「ユーザ認証（APIキー）＋ owner_id 照合」に
  変更した（2026-07-09、設計書 §7.1）。`POST /users` を追加し、`X-Owner-Token`
  ヘッダは廃止（`X-API-Key` / `Authorization: Bearer` に置き換え）。ライフサイクル
  操作（register・データ指定のある execute・delete）は認証必須になり、「トークン
  無し登録＝誰でも操作可」という開発用の緩和は撤廃した。**旧形式
  （`owner_token_hash`）で登録済みの `data_store/` とは互換性がない**: 旧データは
  所有者を持たないため execute/delete できなくなる（status/proof は可）。移行時は
  `data_store/` を空にして登録し直す。
- `POST /execute-wasm`（ステートレス実行）と `POST /data/{id}/execute` は
  **`POST /execute` に統合**した。使用するデータは `?data=<id>` の繰り返しで
  0個以上を可変長指定し（§4.3）、0個がステートレス実行に対応する。旧パスは 404。
- 統合に伴い、単一データ時のマウント名が `/data/input` → `/data/input0` に、
  ステートレス実行のエラー応答が text/plain（`WASM error: ...`）→
  JSON（`{"error":"WASM error: ..."}`）に変わった。成功時の出力整形
  （stdout / stderr 併記）は従来どおり。
- タイムアウト・メモリ上限・ボディ上限 32 MiB はステートレス実行にも適用される
  （旧実装では無制限だった）。
- ルータが `http.ServeMux` パターンになり、未知パスの 404 本文が
  `not found` → `404 page not found`、メソッド不一致が 404 → 405 に変わった。

## 11. 制限事項・設計判断（設計書 §11 との対応）

- **認可**: 利用者＝データオーナー本人であることは owner_id 照合で保証されるように
  なったが、`POST /users` 自体は無認証で誰でも叩ける（ユーザ発行の主体をどう制限
  するかは運用側の課題）。APIキーのローテーション・失効APIも未実装
  （設計書 §11 のとおり別途）。
- **メタデータのロールバック**: Gramine Protected Files は機密性・完全性を守るが
  ロールバック（削除前の meta ファイルのバックアップを後で書き戻す攻撃）は防げない。
  meta には DEK が含まれるため、ホスト管理者が「削除前の meta + blob」を両方保全して
  いた場合、理論上は復元経路が残る。モノトニックカウンタ等による対策は未実装
  （鍵プロビジョニング同様、§11 相当の未解決課題として明示しておく）。
- **DEK のメモリゼロ化は best-effort**: `wipe()` はスライスを上書きするが、Go の
  GC・コピーセマンティクス上、メモリ内に断片が残る可能性は排除できない
  （SGX ではエンクレーブメモリ自体が暗号化されているため実害は限定的）。
- **`deleted_at` の意味**: 削除がリカバリで完遂された場合、`deleted_at` は
  リカバリ時刻（＝実際に消去が完了した時刻）になる。削除リクエスト受理時刻ではない。
- **ボディ上限は定数**（32 MiB）。変更はコード修正が必要。
- **stdout への生データ出力**は設計書どおり許容（利用者＝データオーナー本人前提、
  §3・§8-3・§10）。出力内容の検査は行わない。

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
| `TestRegisterAndStatus` | 登録 → REGISTERED、未知IDは errNotFound |
| `TestExecuteLifecycle` | REGISTERED⇄IN_USE 遷移、IN_USE 中の execute/delete 排他（§5 不変条件2）、復号データの一致 |
| `TestExecuteMultiData` | 複数データの一括取得（指定順で返る・全件同時 IN_USE・重なる実行の排他・失敗時に他データの状態が変わらない all-or-nothing・他オーナー混在の拒否・ゼロ個指定） |
| `TestDeleteIssuesCertificateAndBecomesTerminal` | 証明フィールドの整合、DELETED の終端性（execute 不可・再削除不可・proof 可）、未削除データに proof 無し |
| `TestOwnerEnforcement` | 所有者照合（本人以外の execute/delete → errForbidden） |
| `TestUserCreateAndResolve` / `TestUserPersistenceAcrossRestart` | ユーザ発行（owner_id/APIキーの形式・ハッシュのみ保存）、認証（無効キー → errUnauthorized）、再起動後のユーザ表の保持 |
| `TestConcurrentDeleteRace` | 16並行削除で成功がちょうど1回（TOCTOU） |
| `TestPersistenceAcrossRestart` | 再起動後の状態・所有者・データ・証明の保持 |
| `TestCryptoShreddingOnDisk` | 削除後、blob が消え meta から `dek` が除去されている |
| `TestCrashDuringDeleteRecovery` | DELETING 永続化直後のクラッシュ → 次回起動で削除完遂・証明発行 |
| `TestSealOpenRoundtrip` | AES-GCM 往復、平文非含有、別鍵・改ざんで復号失敗 |
| `TestAPILifecycleFlow` | HTTP 一巡（user → register → execute → delete → proof）+ **証明の署名を検証者の立場で検証**（§8.4 手順3–4） |
| `TestAPIUserAuth` | キー無し/無効キー → 401、他ユーザの execute/delete → 403、所有者混在の execute → 403、Bearer ヘッダでの認証 |
| `TestAPIRegisterValidation` / `TestAPIExecuteBusyConflict` / `TestAPIBodyTooLarge` | ステータスコード対応表（§4.4）の確認 |
| `TestAPIStatelessExecute` / `TestAPIHealth` | データ指定ゼロ個（ステートレス実行）とヘルスチェックの回帰 |
| `TestAPIExecuteMultiData` | HTTP 経由の複数データ指定（重複 400・未知ID混在 404 と他データの状態不変・実行後の全件解放・`input0`/`input1` の内容と順序） |
| `TestAPIExecuteArgs` | `?arg=` の WASI argv 受け渡し（ステートレス/データ併用・空 arg の許容・合計サイズ超過 413） |
| `TestSandboxTimeout` | 無限ループが強制中断される（手組みのループWASM使用） |
| `TestSandboxMemoryLimit` | 上限超過モジュール（2000ページ要求）の拒否 |
| `TestSandboxInputMount` / `TestSandboxMultiInputMount` / `TestSandboxNoInputNoFS` | `/data/input0`, `/data/input1`, ... の可視性・順序（入力あり時のみ） |
| `TestSandboxArgs` / `TestSandboxNoArgs` | WASI argv の受け渡し（`argv[0]="app.wasm"` + 指定順）と、指定なし時に argv を提供しないこと（手組みの argv エコー WASM 使用） |
| `TestSandboxRunsNoopModule` / `TestSandboxRejectsInvalidBinary` / `TestCappedBuffer` | 基本動作・出力上限 |

`wasm_test.go` はタイムアウト・メモリ上限テスト用の最小 WASM バイナリ
（no-op / 無限ループ / 大メモリ要求）を手組みで生成するヘルパ。
