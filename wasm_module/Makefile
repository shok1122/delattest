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

# DATA にデータIDをスペース区切りで並べると data=<id>&data=... を組み立てる。
# DATA を指定する場合は認証必須なので、KEY に APIキー（POST /users で発行）を渡す。
# ARGS に引数をスペース区切りで並べると arg=<v>&arg=... を組み立てる（WASI argv）。
# URLエンコードはしないので、URLに使えない文字は呼び出し側でエンコードすること
# 例: make test-local SRC=passman DATA=d-xxxx ARGS="get github" KEY=ak-...
empty :=
space := $(empty) $(empty)
PARAMS = $(strip $(foreach id,$(DATA),data=$(id)) $(foreach a,$(ARGS),arg=$(a)))
QUERY = $(if $(PARAMS),?$(subst $(space),&,$(PARAMS)))
AUTH_HEADER = $(if $(KEY),-H "X-API-Key: $(KEY)")

.PHONY: test
test:
	curl -X POST "http://delattest.dev:3000/execute$(QUERY)" \
		-H "Content-Type: application/wasm" \
		$(AUTH_HEADER) \
		--data-binary @$(SRC)/app.wasm

.PHONY: test-local
test-local:
	curl -X POST "http://localhost:3000/execute$(QUERY)" \
		-H "Content-Type: application/wasm" \
		$(AUTH_HEADER) \
		--data-binary @$(SRC)/app.wasm

