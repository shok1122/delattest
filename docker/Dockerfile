# =============================================================================
# Stage 1: Chef - cargo-chefのインストールと基本環境のセットアップ
# =============================================================================
# cargo-chefは依存関係とソースコードのビルドを分離することで、
# Dockerのレイヤーキャッシュを効果的に活用し、ビルド時間を大幅に短縮する
FROM rust:1.83-slim AS chef

# cargo-chefのインストール
# これにより、依存関係の情報を抽出・ビルドする機能が使えるようになる
RUN cargo install cargo-chef

# musl-toolsのインストール
# muslはGNU libcの代替となる軽量なC標準ライブラリ実装
# これにより完全な静的リンクが可能になり、実行時の依存関係がなくなる
RUN apt-get update &&   
apt-get install -y musl-tools &&   
rm -rf /var/lib/apt/lists/*

# x86_64-unknown-linux-musl targetの追加
# このターゲットでビルドすることで、muslを使った静的リンクバイナリが生成される
# Gramine-SGXでの実行に最適（依存ライブラリの不一致を回避）
RUN rustup target add x86_64-unknown-linux-musl

# 作業ディレクトリの設定
WORKDIR /build

# =============================================================================
# Stage 2: Planner - 依存関係情報の抽出
# =============================================================================
# このステージでは、Cargo.tomlとCargo.lockから依存関係の情報だけを抽出する
# ソースコード（src/）は含まれないため、コード変更時でもこのレイヤーは再利用される
FROM chef AS planner

# プロジェクト全体をコピー
# ここではCargo.toml、Cargo.lock、src/などすべてが必要
COPY . .

# cargo chef prepareで依存関係情報をrecipe.jsonに抽出
# このファイルには、どのクレート（ライブラリ）が必要かという情報のみが含まれる
RUN cargo chef prepare –recipe-path recipe.json

# =============================================================================
# Stage 3: Builder - 実際のビルド
# =============================================================================
# このステージで依存関係とソースコードを分けてビルドする
FROM chef AS builder

# plannerステージで生成したrecipe.jsonをコピー
# このファイルは依存関係の情報のみを含むため、サイズが小さい
COPY –from=planner /build/recipe.json recipe.json

# cargo chef cookで依存関係だけをビルド
# ここが最も時間のかかる部分だが、recipe.jsonが変わらない限りキャッシュされる
# つまり、依存クレートの追加・削除がない限り、このステップはスキップされる
# –release: 最適化ビルド（実行速度重視）
# –target x86_64-unknown-linux-musl: musl静的リンクターゲット
RUN cargo chef cook -release –target x86_64-unknown-linux-musl –recipe-path recipe.json

# ソースコード全体をコピー
# ここでようやくsrc/main.rsなどの実際のコードがコピーされる
# 依存関係は既にビルド済みなので、自分のコードだけがビルドされる（高速）
COPY . .

# 最終的なビルド
# 依存関係は既にビルド済みなので、このステップは数秒〜数十秒で完了する
# touchコマンドでタイムスタンプを更新し、確実に再ビルドされるようにする
RUN touch src/main.rs &&   
cargo build –release –target x86_64-unknown-linux-musl

# =============================================================================
# Stage 4 (オプション): Runtime - 最小限の実行イメージ
# =============================================================================
# scratchは完全に空のイメージ（0バイト）
# 静的リンクされたバイナリのみを含む最小限のイメージが作成できる
# Gramine-SGXで実行する場合は、バイナリだけを取り出すのでこのステージは不要かもしれない
FROM scratch AS runtime

# ビルドしたバイナリのみをコピー
# your-programの部分はCargo.tomlのnameに合わせて変更すること
# 例: name = “myapp” なら /myapp
COPY –from=builder /build/target/x86_64-unknown-linux-musl/release/your-program /

# エントリーポイントの設定
ENTRYPOINT [”/your-program”]

# =============================================================================
# ビルドと使用方法
# =============================================================================
# 
# 【Dockerイメージのビルド（builderステージまで）】
# docker build –target builder -t rust-sgx-builder .
# 
# 【バイナリを取り出す方法1: コンテナからコピー】
# docker create –name temp rust-sgx-builder
# docker cp temp:/build/target/x86_64-unknown-linux-musl/release/your-program .
# docker rm temp
# 
# 【バイナリを取り出す方法2: ボリュームマウント】
# docker run –rm \
# -v “$(pwd)”:/build \
# -v cargo-cache:/usr/local/cargo/registry \
# rust-sgx-builder \
# sh -c “cargo build –release –target x86_64-unknown-linux-musl”
# 
# ビルド後、target/x86_64-unknown-linux-musl/release/にバイナリが生成される
# 
# 【完全な実行イメージのビルド（runtimeステージまで）】
# docker build -t rust-sgx-app .
# 
# 【イメージからバイナリを抽出】
# docker run –rm rust-sgx-app > your-program
# chmod +x your-program
# =============================================================================
# Gramine-SGXでの使用
# =============================================================================
# 
# 1. このDockerfileでバイナリをビルド
# 2. 生成されたバイナリをGramine-SGXプロジェクトディレクトリにコピー
# 3. manifestファイルを作成（program.manifest.template）
# 4. gramine-manifestでmanifest生成
# 5. gramine-sgx-signで署名
# 6. gramine-sgx で実行
# 
# 詳細は別途manifestファイルのドキュメントを参照
# 
# =============================================================================
