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

# /execute は JSON ボディ {"wasm":"<base64>","data":[...],"args":[...]} を受け取る。
# DATA にデータIDをスペース区切りで並べると "data":[...] を組み立てる。
# DATA を指定する場合は認証必須なので、KEY に APIキー（POST /users で発行）を渡す。
# ARGS に引数をスペース区切りで並べると "args":[...] を組み立てる（WASI argv。
# 1ワード=1引数。引数に空白・二重引用符は使えない）
# 例: make test-local SRC=passman DATA=d-xxxx ARGS="get github" KEY=ak-...
empty :=
space := $(empty) $(empty)
comma := ,
DATA_JSON = $(if $(DATA),$(comma)"data":[$(subst $(space),$(comma),$(strip $(foreach id,$(DATA),"$(id)")))])
ARGS_JSON = $(if $(ARGS),$(comma)"args":[$(subst $(space),$(comma),$(strip $(foreach a,$(ARGS),"$(a)")))])
AUTH_HEADER = $(if $(KEY),-H "X-API-Key: $(KEY)")

define EXEC_CURL
	{ printf '{"wasm":"'; base64 -w0 $(SRC)/app.wasm; printf '"%s}' '$(DATA_JSON)$(ARGS_JSON)'; } \
		| curl -X POST "$(1)/execute" \
			-H "Content-Type: application/json" \
			$(AUTH_HEADER) \
			--data-binary @-
endef

.PHONY: test
test:
	$(call EXEC_CURL,http://delattest.dev:3000)

.PHONY: test-local
test-local:
	$(call EXEC_CURL,http://localhost:3000)

