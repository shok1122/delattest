ld.sh - Rustプログラムをmusl静的リンクでビルドするスクリプト

################################################################################

# 

# 使い方:

# ./scripts/build.sh

# 

# 説明:

# - Dockerを使用してRustプログラムをビルド

# - x86_64-unknown-linux-musl ターゲットで静的リンク

# - cargo-chefでビルド時間を短縮

# 

################################################################################

set -e  # エラーが発生したら即座に終了
set -u  # 未定義変数を使用したらエラー

# 色付き出力用

RED=’\033[0;31m’
GREEN=’\033[0;32m’
YELLOW=’\033[1;33m’
BLUE=’\033[0;34m’
NC=’\033[0m’ # No Color

# ログ出力関数

log_info() {
	echo -e “${BLUE}[INFO]${NC} $1”
}

log_success() {
	echo -e “${GREEN}[SUCCESS]${NC} $1”
}

log_warning() {
	echo -e “${YELLOW}[WARNING]${NC} $1”
}

log_error() {
	echo -e “${RED}[ERROR]${NC} $1”
}

# スクリプトのディレクトリを取得

SCRIPT_DIR=”$(cd “$(dirname “${BASH_SOURCE[0]}”)” && pwd)”
PROJECT_ROOT=”$(cd “$SCRIPT_DIR/..” && pwd)”

# 設定

DOCKER_IMAGE_NAME=“rust-sgx-builder”
DOCKER_BUILD_TARGET=“builder”
BINARY_NAME=“program”  # Cargo.tomlのnameと一致させる

# プロジェクトルートに移動

cd “$PROJECT_ROOT”

log_info “プロジェクトルート: $PROJECT_ROOT”

# Dockerfileの存在確認

if [ ! -f “Dockerfile” ]; then
	log_error “Dockerfileが見つかりません: $PROJECT_ROOT/Dockerfile”
	exit 1
fi

# Cargo.tomlの存在確認

if [ ! -f “Cargo.toml” ]; then
	log_error “Cargo.tomlが見つかりません: $PROJECT_ROOT/Cargo.toml”
	exit 1
fi

# Dockerイメージをビルド

log_info “Dockerイメージをビルド中…”
docker build   
–target “$DOCKER_BUILD_TARGET”   
-t “$DOCKER_IMAGE_NAME”   
. || {
	log_error “Dockerイメージのビルドに失敗しました”
exit 1
}

log_success “Dockerイメージのビルドが完了しました”

# Rustプログラムをビルド

log_info “Rustプログラムをビルド中…”
log_info “ターゲット: x86_64-unknown-linux-musl”

# ボリュームマウントでビルド（キャッシュを活用）

docker run –rm   
-v “$PROJECT_ROOT”:/build   
-v cargo-cache:/usr/local/cargo/registry   
“$DOCKER_IMAGE_NAME”   
sh -c “cargo build –release –target x86_64-unknown-linux-musl” || {
	log_error “Rustプログラムのビルドに失敗しました”
exit 1
}

# ビルド成果物の確認

BINARY_PATH=“target/x86_64-unknown-linux-musl/release/$BINARY_NAME”
if [ ! -f “$BINARY_PATH” ]; then
	log_error “ビルドされたバイナリが見つかりません: $BINARY_PATH”
	exit 1
fi

# バイナリ情報を表示

log_success “ビルドが完了しました”
log_info “バイナリ: $BINARY_PATH”
log_info “サイズ: $(du -h “$BINARY_PATH” | cut -f1)”
log_info “ファイルタイプ: $(file “$BINARY_PATH”)”

# 依存関係の確認（静的リンクの検証）

log_info “依存ライブラリの確認…”
if ldd “$BINARY_PATH” 2>&1 | grep -q “not a dynamic executable”; then
	log_success “✓ 完全に静的リンクされています（依存ライブラリなし）”
else
	log_warning “動的リンクされたライブラリが存在します:”
	ldd “$BINARY_PATH” || true
fi

log_success “すべての処理が完了しました”
echo “”
log_info “次のステップ:”
echo “  1. バイナリを抽出: ./scripts/extract-binary.sh”
echo “  2. ハッシュを計算: ./scripts/verify-binary.sh”
echo “  3. 実行マシンに転送: scp dist/$BINARY_NAME user@sgx-machine:/path/to/runtime/”
