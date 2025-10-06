# Copyright (C) 2023 Gramine contributors
# SPDX-License-Identifier: BSD-3-Clause

APP = delattest

ARCH_LIBDIR ?= /lib/$(shell $(CC) -dumpmachine)

SELF_EXE = target/release/$(APP)

.PHONY: builder
builder:
	docker build --target builder -t rust-sgx-builder .

.PHONY: runner
runner:
	docker build --target runtime -t rust-sgx-runner .

.PHONY: run
run:
	docker run -d --rm \
		-p 3000:3000 \
		--name wasm-runner \
		rust-sgx-runner

.PHONY: stop
stop:
	docker stop wasm-runner

.PHONY: extract
extract:
	docker create --name tmp rust-sgx-builder
	docker cp tmp:/build/target/x86_64-unknown-linux-musl/release/wasm-runner ./cache
	docker rm tmp

.PHONY: upload
upload: extract
	scp ./cache/wasm-runner delattest:~/dev/delattest/cache/wasm-runner

.PHONY: gen-carg-lock
gen-cargo-lock:
	rm -f Cargo.lock
	docker run --rm -v .:/work -w /work rust:1.86-slim \
		cargo generate-lockfile


