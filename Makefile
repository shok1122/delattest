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

.PHONY: test
test:
	curl -X POST http://localhost:3000/execute-wasm \
		-H "Content-Type: application/wasm" \
		--data-binary @wasm_module/hello.wasm

.PHONY: gen-carg-lock
gen-cargo-lock:
	rm -f Cargo.lock
	docker run --rm -v .:/work -w /work rust:1.86-slim \
		cargo generate-lockfile

.PHONY: all
all: $(SELF_EXE) $(APP).manifest
ifeq ($(SGX),1)
all: $(APP).manifest.sgx $(APP).sig
endif

ifeq ($(DEBUG),1)
GRAMINE_LOG_LEVEL = debug
else
GRAMINE_LOG_LEVEL = error
endif

# Note that we're compiling in release mode regardless of the DEBUG setting passed
# to Make, as compiling in debug mode results in an order of magnitude's difference in
# performance that makes testing by running a benchmark with ab painful. The primary goal
# of the DEBUG setting is to control Gramine's loglevel.
-include $(SELF_EXE).d # See also: .cargo/config.toml
$(SELF_EXE): Cargo.toml
	cargo build --release

$(APP).manifest: $(APP).manifest.template $(SELF_EXE)
	gramine-manifest \
		-Dlog_level=$(GRAMINE_LOG_LEVEL) \
		-Darch_libdir=$(ARCH_LIBDIR) \
		-Dself_exe=$(SELF_EXE) \
		$< $@

$(APP).manifest.sgx $(APP).sig &: $(APP).manifest
	gramine-sgx-sign \
		--manifest $< \
		--output $<.sgx

ifeq ($(SGX),)
GRAMINE = gramine-direct
else
GRAMINE = gramine-sgx
endif

#.PHONY: start-gramine-server
#run: all
#	$(GRAMINE) $(APP)

.PHONY: clean
clean:
	$(RM) -rf *.sig *.manifest.sgx *.manifest OUTPUT

.PHONY: distclean
distclean: clean
	$(RM) -rf target/ Cargo.lock
