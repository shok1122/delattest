# delattest システム説明書

本書は delattest リポジトリが実現するシステムの全体像を説明する入門ドキュメント
（システム説明書）である。初めてこのリポジトリに触れる人が、システムの目的・
構成要素・セキュリティモデル・使い方を一通り理解できることを目的とする。

各トピックの詳細は以下の既存ドキュメントに委ねる（本書はその入口となる要約）。

| ドキュメント | 内容 |
|---|---|
| `README.md` | 新規マシンでのセットアップ手順・API 一覧・リセット手順・トラブルシューティング |
| `wasm_runner/SPEC.md` | wasm-runner の実装仕様書（as-built。コードと突き合わせて読む詳細仕様） |
| `docs/260709-data-lifecycle-spec.md` | データライフサイクル管理機能の設計仕様書（脅威モデル・削除証明の設計） |
| `docs/260710-signature-auth-and-program-upload-workplan.md` | Ed25519 署名認証・プログラム事前アップロード導入の作業計画書（設計論点の記録） |
| `wasm_module/passman/README.md` | サンプル WASM モジュール（パスワードマネージャ）の説明 |

---

## 1. システム概要

delattest（**del**etion **attest**ation）は、**「預けたデータが確かに削除された」
ことを第三者が検証できる形で証明する**（削除証明: Deletion Certificate）ための
実験システムである。

中核は `wasm-runner` という HTTP サーバーで、Intel SGX の TEE（Trusted Execution
Environment）内で Gramine（libOS）を使って動作する。利用者は次のことができる。

- **データの登録**: データを暗号化された封印ストレージに預ける。
- **データの利用**: 事前にアップロードした WASM プログラムを、預けたデータを
  入力としてエンクレーブ内のサンドボックスで実行し、計算結果だけを受け取る。
- **データの削除**: データを復元不能に削除し、SGX リモートアテステーションに
  裏付けられた**削除証明**（JSON）を受け取る。

ポイントは、データの登録から削除までの**ライフサイクル全体**が TEE の保護下で
管理されることである。データの平文はエンクレーブの外（ホスト OS、ディスク、
インフラ管理者）に一切露出せず、実行中の WASM プログラムはネットワークに
アクセスできないため、データの複製を作る経路が存在しない。その状態で削除が
行われるからこそ、「削除された＝もうどこにも存在しない」ことに意味が生まれる。

### 技術スタック

| 層 | 技術 |
|---|---|
| TEE | Intel SGX + Gramine（gramine-sgx / 開発時は gramine-direct） |
| サーバー本体 | Go（net/http）。CGO 無効の静的バイナリとして Docker でビルド |
| WASM 実行 | wazero（Pure Go の WebAssembly ランタイム、WASI preview1） |
| 封印ストレージ | Gramine encrypted mount（層1）+ データごとの AES-256-GCM（層2） |
| 認証 | Ed25519 リクエスト署名（API キーは廃止済み） |
| クライアント | `scripts/runner-cli.sh`（bash + curl + openssl 3.x） |
| WASM モジュール開発 | C + wasi-sdk（Docker イメージ `wasi-build`） |

---

## 2. 登場人物と信頼モデル

本システムには 3 種類の主体が登場する。主体は事前登録された ID ではなく
**Ed25519 鍵ペア**（公開鍵そのもの）で識別される。

| 主体 | 役割 | 認証 |
|---|---|---|
| **ランナーオーナー（オーナー）** | wasm-runner を自身のためにデプロイ・運用し、WASM を実行する主体。実行結果（stdout）を受け取る | デプロイ直後に TOFU（Trust On First Use）で公開鍵を 1 つ登録。プログラム管理・execute はオーナー鍵専権 |
| **アップローダ（データ提供者）** | データを登録する主体。オーナーと同一人物でも別人でもよい | 任意の Ed25519 鍵で署名して登録（事前登録不要）。署名した公開鍵がアップローダとして記録される |
| **第三者検証者** | 削除証明を受け取り、DCAP で quote を検証する監査者（検証ツール自体は本リポジトリのスコープ外） | 不要（`status` / `proof` は認証なしで取得可能） |

### 信頼の前提

- SGX のハードウェア保証と Gramine の実装を信頼する。
- **ホスト OS・インフラ管理者（root 含む）・ネットワーク上の攻撃者は信頼しない**
  （脅威主体）。wasm-runner バイナリの真正性はリモートアテステーションで検証できる。
- **アップローダはオーナーを無条件には信頼しない**。アップローダが許可するのは
  「自分が監査したコード（コンテンツハッシュで特定）が自分のデータを読むこと」
  だけであり、これはデータごとのホワイトリスト（§5）でシステム的に強制される。
  ただし、許可したプログラムの出力はオーナーに開示されることをアップローダは
  了解する（生データをそのまま出力するプログラムを許可すれば、実質的に生データを
  開示したことになる）。

脅威モデルの全体は設計書 `docs/260709-data-lifecycle-spec.md` §10 を参照。

---

## 3. アーキテクチャ

```
                        ┌───────────────────────────────────────────────────┐
                        │        wasm-runner (Gramine-SGX enclave)          │
                        │                                                   │
 Client                 │  ┌─────────────────┐   ┌───────────────────────┐  │
 (owner / uploader)     │  │ Lifecycle       │   │ Secure Storage        │  │
 ──── HTTP ───────────> │  │ Manager         │<─>│ (encrypted mount +    │  │
  署名付きコマンド       │  │ (状態機械・排他) │   │  AES-256-GCM)         │  │
                        │  └──────┬──────────┘   └───────────────────────┘  │
                        │         │                                         │
                        │         V                                         │
                        │  ┌─────────────────┐   ┌───────────────────────┐  │
                        │  │ WASM Execution  │   │ Attestation /         │  │
                        │  │ Sandbox         │   │ Deletion Proof        │  │
                        │  │ (wazero,        │   │ (SGX quote 取得・      │  │
                        │  │  ネットワーク遮断) │   │  削除証明への署名)      │  │
                        │  └─────────────────┘   └───────────────────────┘  │
                        └───────────────────────────────────────────────────┘
```

wasm-runner プロセス全体がエンクレーブ内で動作するため、上記 4 コンポーネントの
すべてが TEE の保護下にある。外部との接点は HTTP（既定 `0.0.0.0:3000`）のみ。

| コンポーネント | 実装ファイル | 責務 |
|---|---|---|
| HTTP API / 認可 | `wasm_runner/handlers.go` | ルーティング・リクエスト検証・エラー応答 |
| 署名認証 | `wasm_runner/auth.go` | Ed25519 署名検証・タイムスタンプウィンドウ・リプレイ対策 |
| オーナー管理 | `wasm_runner/owner.go` | オーナー鍵の TOFU 登録・永続化 |
| プログラムレジストリ | `wasm_runner/programs.go` | WASM の事前アップロード（コンテンツアドレス ID）・整合性検証 |
| Lifecycle Manager | `wasm_runner/lifecycle.go` | 状態機械・排他制御・ホワイトリスト照合・クラッシュリカバリ |
| 封印ストレージ | `wasm_runner/storage.go` | ファイル配置・AES-256-GCM・原子的書き込み |
| WASM サンドボックス | `wasm_runner/sandbox.go` | wazero 実行・実行制約（§6） |
| 削除証明 | `wasm_runner/attestation.go` | 署名鍵生成・SGX quote 取得・証明の発行 |

---

## 4. データのライフサイクルと削除証明

### 状態遷移

```
               register
 (nonexistent) ────────> REGISTERED ⇄ IN_USE（execute 実行中のみ）
                             │
                             │ delete
                             V
                         DELETING ────> DELETED（終端。ID 再利用不可）
                      （鍵破棄 + 削除証明の発行）
```

- `execute` は複数データを同時指定でき、指定した全データが**原子的
  （all-or-nothing）**に `IN_USE` になる。1 件でも存在しない・削除済み・使用中・
  ホワイトリスト外なら、どのデータの状態も変えずにエラーを返す。
- `IN_USE` / `DELETING` 中の競合操作は排他ロックでブロックされる（TOCTOU 防止）。
- 生データ（平文）を返す API は**一切存在しない**。取り出せるのは WASM プログラム
  の計算結果（stdout）のみ。
- プロセスがクラッシュしても、再起動時に中断状態（`DELETING` など）をリカバリして
  削除を完遂する。

### 削除の実体: クリプトシュレッディング

削除は「暗号化 blob の消去」に加えて「データ暗号鍵（DEK）とメタデータの破棄」で
行う。鍵が破棄されれば、バックアップ等に暗号文が残存しても復号不能＝実質的に
削除されたことになる。

### 削除証明

削除時に発行される JSON。以下を含む。

```json
{
  "data_id": "d-xxxxxxxx",
  "deleted_at": "2026-07-08T12:34:56Z",
  "content_hash": "sha256:...（登録時点の元データのハッシュ）",
  "enclave_report": { "mrenclave": "...", "mrsigner": "...", "quote": "base64:..." },
  "signature": "base64:...（エンクレーブ内で生成された鍵による Ed25519 署名）"
}
```

検証者は (1) quote を DCAP で検証、(2) quote の `user_report_data` に埋め込まれた
公開鍵ハッシュと証明の公開鍵の一致を確認、(3) `signature` を検証、
(4) `content_hash` を登録データの sha256 と照合する（設計書 §9.3）。
quote はエンクレーブ起動時に一度だけ生成され、以後の証明はその鍵で署名される。

`RA_TYPE=none` や gramine-direct では quote は空になり、署名のみの証明
（開発用・第三者検証不可）に縮退する。

---

## 5. セキュリティの仕組み

### 5.1 Ed25519 リクエスト署名認証

状態を変える操作（コマンド）はすべて署名必須。リクエストに以下のヘッダを付け、
`<METHOD>\n<PATH>\n<timestamp>\nsha256(body)` への Ed25519 署名を検証する。

```
X-Public-Key: <公開鍵 raw 32B の base64>
X-Signature:  <署名の base64>
X-Timestamp:  <RFC3339 UTC（ナノ秒精度推奨）>
```

リプレイ対策はタイムスタンプ許容ウィンドウ（既定 300 秒）+ 使用済み署名キャッシュ。
手作業で署名を組む必要はなく、`scripts/runner-cli.sh` がすべて代行する。

認証（署名が正当か = 401）と認可（その鍵にその操作が許されるか = 403）は分離
されている。操作ごとに要求される鍵は次のとおり。

| 操作 | 必要な鍵 |
|---|---|
| プログラム登録・削除、execute | オーナー鍵のみ |
| データ登録 | 任意の鍵（署名者がアップローダとして記録される） |
| ホワイトリスト編集 | 当該データのアップローダ鍵のみ（**オーナー鍵では不可**） |
| データ削除 | オーナー鍵またはアップローダ鍵 |
| ヘルスチェック・status・proof | 不要 |

### 5.2 プログラムのコンテンツアドレスとホワイトリスト

- WASM プログラムは実行前に `POST /programs` でアップロードする。ID は
  **`p-<sha256(バイナリ)>` のコンテンツアドレス**であり、同一バイナリの
  再アップロードは冪等に同じ ID を返す。ID はクライアント側でも計算できる
  （`runner-cli.sh program-id`）ため、ID の発行をサーバに信頼委譲する必要がない。
- データは登録時に `allowed_programs`（実行を許可する program_id の集合）を持つ。
  **既定は空 = すべて拒否（deny by default）**。
- execute の認可は 2 段階: (1) 署名がオーナー鍵、(2) 指定した各データについて
  `program_id ∈ allowed_programs`。
- program_id がコード内容と 1 対 1 なので、「許可」は**特定のコード内容の承認**を
  意味する。アテステーション（ランナー自体の検証）と組み合わせると、アップローダは
  「自分が監査・承認したコードだけが自分のデータを読める」ことをエンドツーエンドで
  保証できる。

### 5.3 封印ストレージ（二層の暗号化）

| 層 | 仕組み | 対象 |
|---|---|---|
| 層1 | Gramine encrypted mount（ホスト上の `data_store/` は常に暗号化状態） | `DATA_DIR` 以下すべて |
| 層2 | データごとの DEK による AES-256-GCM（削除時に鍵ごと破棄 = クリプトシュレッディング） | データ本体（blob）のみ |

封印鍵は SGX では MRSIGNER 由来のシーリング鍵、gramine-direct では開発用固定鍵。
**モードを跨いで同じ `data_store/` は読めない**（切り替え時は空にする）。

### 5.4 WASM 実行サンドボックス

- **ネットワーク系のホスト関数を一切提供しない** — WASM からの外部送信は不可能。
  これが「複製が作られない」ことの根幹であり、将来もネットワーク系ホスト関数を
  追加しないことが設計原則。
- ファイルシステムは、execute で指定した登録データが指定順に `/data/input0`,
  `/data/input1`, ... として**読み取り専用**で見えるのみ（メモリ上の FS。
  平文がホストのディスクに書かれることはない）。書き込みマウントは無い。
- 実行タイムアウト（既定 30 秒）・メモリ上限（既定 64 MiB）・stdout/stderr 各
  1 MiB 上限（DoS 対策）。

---

## 6. API リファレンス（要約）

詳細（ボディ形式・ステータスコード対応表）は `README.md` の API 節と
`wasm_runner/SPEC.md` §4 を参照。

| メソッド / パス | 認証 | 説明 |
|---|---|---|
| `GET /` | 不要 | ヘルスチェック（エンドポイント一覧を返す） |
| `POST /owner` | 不要（初回のみ） | オーナー公開鍵の TOFU 登録。登録済みは 409。未登録の間は全コマンドが 403 |
| `POST /programs` | オーナー | WASM の事前アップロード。応答: `{program_id, uploaded_at}`。冪等 |
| `DELETE /programs/{id}` | オーナー | プログラム削除（再アップロードで同 ID に復元可能） |
| `POST /execute` | オーナー | WASM 実行。ボディ: `{program_id, data:[...], args:[...]}`。応答: stdout |
| `POST /data` | 任意の鍵 | データ登録。ボディ: `{data:<base64>, allowed_programs:[...]}`。応答: `{data_id, registered_at}` |
| `PUT /data/{id}/programs` | アップローダ | ホワイトリストの全置換（空配列で「すべて拒否」に戻せる） |
| `DELETE /data/{id}` | オーナー or アップローダ | データ削除。応答: 削除証明（JSON） |
| `GET /data/{id}/status` | 不要 | 現在の状態（REGISTERED / IN_USE / DELETING / DELETED） |
| `GET /data/{id}/proof` | 不要 | DELETED データの削除証明の再取得（監査用） |

### クライアント: runner-cli.sh

依存は curl / openssl 3.x / coreutils のみ。典型的な一巡:

```sh
CLI=scripts/runner-cli.sh

$CLI keygen owner.pem                                  # 鍵生成
$CLI -k owner.pem owner-register --wait 30             # 起動直後に TOFU 登録
$CLI -k owner.pem program-upload app.wasm              # → p-<sha256>
$CLI -k uploader.pem data-upload input.bin p-<sha256>  # データ登録 + ホワイトリスト
$CLI -k owner.pem execute p-<sha256> -d d-<id> -a arg1 # 実行
$CLI -k uploader.pem data-delete d-<id>                # 削除 → 削除証明
$CLI proof d-<id>                                      # 証明の再取得
```

---

## 7. リポジトリ構成

```
wasm_runner/        wasm-runner 本体（Go）。Docker で CGO 無効の静的バイナリをビルド
                    実装仕様書: wasm_runner/SPEC.md、Go ユニットテスト一式
wasm_module/        サンプル WASM モジュール（C + wasi-sdk でビルド）
  hello1, hello2      "hello" を出力するだけの疎通確認用
  readinput           /data/input0, input1, ... を連結して stdout に出す動作確認用
  passman             登録データをパスワード帳として使うデモ（ライフサイクル一巡の実例）
docs/               設計仕様書・作業計画書・本書
scripts/
  runner-cli.sh       署名付きリクエストを送るクライアント CLI
  build.sh            ビルド補助
Makefile            リポジトリ直下。gramine-manifest / gramine-sgx-sign / 実行
wasm-runner.manifest.template   Gramine マニフェストのテンプレート
cache/wasm-runner   実行バイナリの置き場（.gitignore 対象。ビルドして生成）
data_store/         封印ストレージ（.gitignore 対象。ホスト上では常に暗号化状態）
```

---

## 8. ビルド・デプロイ・運用

新規マシンでの詳細手順は `README.md` を参照。流れは以下のとおり。

1. **Gramine のインストール**（公式 apt リポジトリ）と SGX 署名鍵の生成
   （`gramine-sgx-gen-private-key`、初回のみ）。
2. **Docker のインストール**（ビルドに使用。ホストに Go は不要）。
3. **バイナリのビルド**: `make -C wasm_runner install`
   （Docker ビルド → バイナリ取り出し → `cache/` へ配置）。
4. **マニフェスト生成と起動**:

   ```sh
   SGX=1 make        # マニフェスト生成 + SGX 署名
   SGX=1 make run    # gramine-sgx で起動（0.0.0.0:3000）
   ```

   `SGX=1` を外すと gramine-direct（非 SGX、開発用）。削除証明に quote を含める
   には DCAP スタックのあるホストで `SGX=1 RA_TYPE=dcap make`（既定は `none`）。
5. **オーナー登録**: 起動直後に**即座に**
   `scripts/runner-cli.sh -k owner.pem owner-register --wait 30` を実行する。
   起動と登録は一連の手順として行うこと（TOFU 先取りリスク対策）。

### WASM モジュールのビルドと動作確認

```sh
cd wasm_module
docker build -t wasi-build .              # ビルド環境イメージ（初回のみ）
make -C hello1 all                        # main.c → app.wasm
make test-local SRC=hello1 KEY=../owner.pem   # アップロード + 実行（localhost:3000）
make test SRC=hello1 KEY=../owner.pem         # 本番ドメイン（delattest.dev:3000）宛て
```

### 運用上の注意

- `SGX=1` の有無や `RA_TYPE` を切り替えるときは `make clean` → マニフェスト再生成。
  さらに**モードを跨ぐと封印鍵が変わり `data_store/` が読めなくなる**ため、
  中身を空にしてオーナー登録からやり直す。
- `wasm_runner/` のソースを更新したら `make -C wasm_runner install` で再ビルドする
  （古い `cache/wasm-runner` のまま起動すると新 API が 404 になる）。
- クリーンな状態に戻す（TOFU をやり直す）手順とトラブルシューティングは
  `README.md` の該当節を参照。

### 主な環境変数

| 変数 | 既定値 | 説明 |
|---|---|---|
| `HOST` / `PORT` | `0.0.0.0` / `3000` | 待ち受けアドレス |
| `DATA_DIR` | `data_store` | 封印ストレージの場所（Gramine 実行時はマニフェストが `/data_store` を指定） |
| `AUTH_WINDOW_SEC` | `300` | 署名タイムスタンプの許容ウィンドウ（秒） |
| `EXEC_TIMEOUT_SEC` | `30` | WASM 実行タイムアウト（秒） |
| `WASM_MEM_LIMIT_PAGES` | `1024` | WASM メモリ上限（64 KiB ページ数 = 64 MiB） |

### テスト

ホストに Go は不要。Docker で実行する。

```sh
cd wasm_runner
docker run --rm -v "$PWD":/work -w /work golang:1.25-bookworm go test ./... -count=1
```

状態遷移・排他制御・永続化・クラッシュリカバリ・署名検証・実行制約を網羅する
ユニットテスト（40 件超）がある。テスト一覧は `wasm_runner/SPEC.md` §12。

---

## 9. 制限事項・既知の課題

詳細は `wasm_runner/SPEC.md` §11 と設計書 §11 を参照。主なもの:

- **TOFU の先取りリスク**: `POST /owner` は認証不要のため、デプロイから登録までの
  間に攻撃者が先に鍵を登録できる。対策は「起動 → 即登録」の運用のみ（先取りされる
  と全コマンドが 403/409 になるため即検知でき、`data_store/` を消して再デプロイ
  すればよい）。
- **時計の信頼**: タイムスタンプ検証の時計はホスト由来。リプレイキャッシュも
  メモリ上のみで、再起動を跨ぐとウィンドウ幅ぶんの再利用余地がある。
- **メタデータのロールバック**: Gramine Protected Files はホストによるファイル
  巻き戻し（削除前の meta や `owner.json` の書き戻し）を防げない。モノトニック
  カウンタ等は未実装。
- **鍵のローテーション**: オーナー鍵・アップローダ鍵の変更・失効 API は未実装。
- **出力の開示範囲**: 許可したプログラムの出力はオーナーに渡る。出力内容の検査
  （DLP 的な仕組み）は原理的に不完全なため導入しない — 保護はホワイトリストによる
  「実行できるコードの事前承認」で行う。
- **第三者検証者ツール**: 削除証明の検証側の実装は本リポジトリのスコープ外。
- **DCAP 環境**: `RA_TYPE=dcap` はホスト側の DCAP 基盤（aesmd, quote provider）が
  必要。2026-07 時点の検証マシンでは AESM が error 12 を返し未確認。

---

## 10. 用語集

| 用語 | 意味 |
|---|---|
| TEE | Trusted Execution Environment。本プロジェクトでは Gramine-SGX のエンクレーブ |
| エンクレーブ | SGX が提供する保護されたメモリ領域・実行環境。ホスト OS からも中身を見られない |
| Gramine | 既存の Linux バイナリを SGX エンクレーブ内で動かす libOS。gramine-direct は SGX なしのエミュレーション実行 |
| リモートアテステーション | エンクレーブの真正性（どのコードが動いているか）を第三者が検証できる SGX の仕組み。DCAP はその検証基盤 |
| quote | アテステーションの証拠となる SGX の署名付きレポート |
| MRENCLAVE / MRSIGNER | エンクレーブの計測値 / 署名者の識別値。quote に含まれる |
| 封印ストレージ | TEE 管理下で暗号化保存されるデータストア（`data_store/`） |
| DEK | Data Encryption Key。データ 1 件ごとの暗号鍵。削除時に破棄される |
| クリプトシュレッディング | 暗号鍵を破棄することで暗号化データを復元不能にする削除手法 |
| 削除証明 | データが削除されたことを TEE が署名して証明する電子的な証跡 |
| TOFU | Trust On First Use。最初に登録された鍵をそのまま信頼する方式 |
| program_id | `p-<sha256(バイナリ)>` 形式のコンテンツアドレス。WASM プログラムの ID |
| ホワイトリスト（allowed_programs） | データごとの「実行を許可する program_id の集合」。編集はアップローダ専権 |
| WASI | WebAssembly System Interface。WASM から OS 機能（ファイル・引数等）を使う標準 API |
| wazero | Pure Go の WebAssembly ランタイム |
