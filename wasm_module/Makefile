SUBDIRS := hello1 hello2
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

.PHONY: test
test:
	curl -X POST http://delattest.dev:3000/execute-wasm \
		-H "Content-Type: application/wasm" \
		--data-binary @$(SRC)/app.wasm

.PHONY: test-local
test-local:
	curl -X POST http://localhost:3000/execute-wasm \
		-H "Content-Type: application/wasm" \
		--data-binary @$(SRC)/app.wasm

