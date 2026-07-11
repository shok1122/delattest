#!/usr/bin/env bash
################################################################################
# runner-cli.sh - wasm_runner クライアント（Ed25519 署名付きリクエストの補助）
#
# 依存: bash, curl, openssl 3.x, sha256sum, base64, mktemp（追加インストール不要）
#
# 使い方:
#   scripts/runner-cli.sh [-u URL] [-k KEY.pem] <コマンド> [引数...]
#
# 鍵管理:
#   keygen <key.pem>                Ed25519 鍵ペアを生成（秘密鍵 PEM、0600）
#   pubkey <key.pem>                公開鍵（raw 32B の base64）を表示
#   program-id <file.wasm>          ローカルで program_id（p-<sha256>）を計算
#
# デプロイ（起動 → オーナー登録を一連で行う。§3.1 案B: TOFU）:
#   1. サーバを起動する:            make run  （または SGX=1 make run）
#   2. 直後に即座に登録する:        runner-cli.sh -k owner.pem owner-register --wait 30
#      （--wait N: サーバが応答するまで最大 N 秒ポーリングしてから登録する。
#       初回登録リクエストの先取りリスクを縮めるため、起動と登録は続けて行うこと）
#
# コマンド（署名必須。-k で署名鍵を指定）:
#   owner-register [--wait N]       オーナー公開鍵の登録（登録自体は認証不要・初回のみ）
#   program-upload <file.wasm>      WASM プログラムの事前アップロード（オーナー鍵）
#   program-delete <p-id>           プログラムの削除（オーナー鍵）
#   data-upload <file> [p-id ...]   データ登録＋実行許可プログラムの指定（任意の鍵）
#   data-programs <d-id> [p-id ...] ホワイトリストの全置換（アップローダ鍵のみ）
#   data-delete <d-id>              データ削除（オーナー鍵 or アップローダ鍵）
#   execute <p-id> [-d d-id]... [-a arg]...   WASM 実行（オーナー鍵）
#   req <METHOD> <PATH> [body-file] 汎用の署名付きリクエスト
#
# 認証不要:
#   health                          GET /
#   status <d-id>                   GET /data/{id}/status
#   proof <d-id>                    GET /data/{id}/proof
#
# 署名プロトコル（wasm_runner/SPEC.md 参照）:
#   メッセージ = "<METHOD>\n<PATH>\n<X-Timestamp>\n<sha256(body) hex>"
#   ヘッダ     = X-Public-Key / X-Signature / X-Timestamp
#   タイムスタンプはナノ秒精度の RFC3339（同一秒内の同一コマンド再送が
#   リプレイ判定と衝突しないようにするため）
#
# 環境変数: WASM_RUNNER_URL（既定 http://localhost:3000。-u が優先）
################################################################################

set -euo pipefail

URL="${WASM_RUNNER_URL:-http://localhost:3000}"
KEY=""

die() {
	echo "error: $*" >&2
	exit 1
}

usage() {
	sed -n '3,42p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
}

# 公開鍵の raw 32B を base64 で得る。Ed25519 の SubjectPublicKeyInfo(DER) は
# 12 バイトのヘッダ + raw 公開鍵 32 バイトなので、末尾 32 バイトが raw 公開鍵
pubkey_b64() {
	openssl pkey -in "$1" -pubout -outform DER | tail -c 32 | base64 -w0
}

# JSON 文字列値のエスケープ（バックスラッシュと二重引用符のみ。
# 改行等の制御文字を含む引数は非対応）
json_escape() {
	sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# 署名付きリクエストを送る: signed_request METHOD PATH BODYFILE
# （ボディ無しのコマンドは空ファイルを渡す。署名対象は空バイト列のハッシュ）
signed_request() {
	local method=$1 path=$2 bodyfile=$3
	[ -n "$KEY" ] || die "signing key required: use -k <key.pem>"
	[ -f "$KEY" ] || die "key file not found: $KEY"

	local ts bodyhash msgfile sig
	ts=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
	case $ts in
	*N*) ts=$(date -u +%Y-%m-%dT%H:%M:%SZ) ;; # %N 非対応の date（BSD等）は秒精度に落とす
	esac
	bodyhash=$(sha256sum "$bodyfile" | cut -d' ' -f1)
	msgfile=$(mktemp)
	printf '%s\n%s\n%s\n%s' "$method" "$path" "$ts" "$bodyhash" >"$msgfile"
	sig=$(openssl pkeyutl -sign -inkey "$KEY" -rawin -in "$msgfile" | base64 -w0)
	rm -f "$msgfile"

	curl -sS -X "$method" "$URL$path" \
		-H "Content-Type: application/json" \
		-H "X-Public-Key: $(pubkey_b64 "$KEY")" \
		-H "X-Signature: $sig" \
		-H "X-Timestamp: $ts" \
		--data-binary "@$bodyfile"
	echo
}

# program_id の列を JSON 配列の中身（"p-a","p-b"）に組み立てる
programs_json() {
	local out="" id
	for id in "$@"; do
		out+=",\"$id\""
	done
	printf '%s' "${out#,}"
}

cmd_keygen() {
	local out=${1:?usage: keygen <key.pem>}
	[ -e "$out" ] && die "refusing to overwrite existing $out"
	openssl genpkey -algorithm ed25519 -out "$out"
	chmod 600 "$out"
	echo "private key: $out" >&2
	echo "public key : $(pubkey_b64 "$out")" >&2
}

cmd_owner_register() {
	[ -n "$KEY" ] || die "signing key required: use -k <key.pem>"
	if [ "${1:-}" = "--wait" ]; then
		local deadline=$((SECONDS + ${2:-30}))
		until curl -sf -o /dev/null "$URL/"; do
			[ $SECONDS -lt $deadline ] || die "server did not come up at $URL"
			sleep 1
		done
	fi
	# 登録自体は認証不要（TOFU）。デプロイ直後に即座に呼ぶこと
	curl -sS -X POST "$URL/owner" \
		-H "Content-Type: application/json" \
		-d "{\"public_key\":\"$(pubkey_b64 "$KEY")\"}"
	echo
}

cmd_program_upload() {
	local wasm=${1:?usage: program-upload <file.wasm>}
	[ -f "$wasm" ] || die "file not found: $wasm"
	echo "local program_id: p-$(sha256sum "$wasm" | cut -d' ' -f1)" >&2
	local body
	body=$(mktemp)
	printf '{"wasm":"%s"}' "$(base64 -w0 <"$wasm")" >"$body"
	signed_request POST /programs "$body"
	rm -f "$body"
}

cmd_data_upload() {
	local file=${1:?usage: data-upload <file> [p-id ...]}
	[ -f "$file" ] || die "file not found: $file"
	shift
	local body
	body=$(mktemp)
	printf '{"data":"%s","allowed_programs":[%s]}' \
		"$(base64 -w0 <"$file")" "$(programs_json "$@")" >"$body"
	signed_request POST /data "$body"
	rm -f "$body"
}

cmd_data_programs() {
	local id=${1:?usage: data-programs <d-id> [p-id ...]}
	shift
	local body
	body=$(mktemp)
	printf '{"allowed_programs":[%s]}' "$(programs_json "$@")" >"$body"
	signed_request PUT "/data/$id/programs" "$body"
	rm -f "$body"
}

cmd_execute() {
	local pid=${1:?usage: execute <p-id> [-d d-id]... [-a arg]...}
	shift
	local data="" args=""
	while [ $# -gt 0 ]; do
		case $1 in
		-d)
			data+=",\"${2:?-d requires a data id}\""
			shift 2
			;;
		-a)
			args+=",\"$(printf '%s' "${2?-a requires a value}" | json_escape)\""
			shift 2
			;;
		*) die "unknown execute option: $1" ;;
		esac
	done
	local body
	body=$(mktemp)
	printf '{"program_id":"%s","data":[%s],"args":[%s]}' \
		"$pid" "${data#,}" "${args#,}" >"$body"
	signed_request POST /execute "$body"
	rm -f "$body"
}

cmd_req() {
	local method=${1:?usage: req <METHOD> <PATH> [body-file]}
	local path=${2:?usage: req <METHOD> <PATH> [body-file]}
	local bodyfile=${3:-}
	local tmp=""
	if [ -z "$bodyfile" ]; then
		tmp=$(mktemp)
		bodyfile=$tmp
	fi
	signed_request "$method" "$path" "$bodyfile"
	[ -n "$tmp" ] && rm -f "$tmp"
}

cmd_delete() {
	local kind=$1 id=${2:?usage: ${3}-delete <id>}
	local empty
	empty=$(mktemp)
	signed_request DELETE "/$kind/$id" "$empty"
	rm -f "$empty"
}

# ---- メイン ------------------------------------------------------------------

while getopts "u:k:h" opt; do
	case $opt in
	u) URL=$OPTARG ;;
	k) KEY=$OPTARG ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))
[ $# -ge 1 ] || usage
cmd=$1
shift

case $cmd in
keygen) cmd_keygen "$@" ;;
pubkey) pubkey_b64 "${1:?usage: pubkey <key.pem>}" && echo ;;
program-id) echo "p-$(sha256sum "${1:?usage: program-id <file.wasm>}" | cut -d' ' -f1)" ;;
owner-register) cmd_owner_register "$@" ;;
program-upload) cmd_program_upload "$@" ;;
program-delete) cmd_delete programs "${1:-}" program ;;
data-upload) cmd_data_upload "$@" ;;
data-programs) cmd_data_programs "$@" ;;
data-delete) cmd_delete data "${1:-}" data ;;
execute) cmd_execute "$@" ;;
req) cmd_req "$@" ;;
health) curl -sS "$URL/" && echo ;;
status) curl -sS "$URL/data/${1:?usage: status <d-id>}/status" && echo ;;
proof) curl -sS "$URL/data/${1:?usage: proof <d-id>}/proof" && echo ;;
*) die "unknown command: $cmd (run without arguments for usage)" ;;
esac
