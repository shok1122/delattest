SUBDIRS := hello1 hello2 readinput passman
ifeq ($(SRC),)
    TARGET_DIRS := $(SUBDIRS)
else
    TARGET_DIRS := $(SRC)
endif

.PHONY: all
all:
	@for dir in $(TARGET_DIRS); do \
		echo "=== Building $$dir ==="; \
		$(MAKE) -C $$dir all || exit $$?; \
	done

.PHONY: build
build:
	docker build -t wasi-build .

# /execute は事前アップロード済みプログラムを program_id（p-<sha256>）で参照する。
# すべてのコマンドは Ed25519 署名認証が必須なので、KEY にオーナー秘密鍵
# （scripts/runner-cli.sh keygen で生成した PEM）を渡す。
# アップロードは冪等（同一バイナリ→同一ID）なので毎回実行してよい。
# DATA にデータIDをスペース区切りで並べると -d の繰り返しに展開される
# （各データのホワイトリストに当該 program_id が入っていること）。
# ARGS も同様に -a に展開される（WASI argv。1ワード=1引数。空白・二重引用符は不可）。
# 例: make test-local SRC=passman KEY=../owner.pem DATA=d-xxxx ARGS="get github"
CLI := ../scripts/runner-cli.sh

define EXEC_CLI
	@test -n "$(KEY)" || { echo "KEY=<owner key .pem> is required (generate: $(CLI) keygen owner.pem)" >&2; exit 1; }
	$(CLI) -u $(1) -k $(KEY) program-upload $(SRC)/app.wasm
	$(CLI) -u $(1) -k $(KEY) execute $$($(CLI) program-id $(SRC)/app.wasm) \
		$(foreach id,$(DATA),-d $(id)) $(foreach a,$(ARGS),-a $(a))
endef

.PHONY: test
test:
	$(call EXEC_CLI,http://delattest.dev:3000)

.PHONY: test-local
test-local:
	$(call EXEC_CLI,http://localhost:3000)

