# Copyright (C) 2023 Gramine contributors
# SPDX-License-Identifier: BSD-3-Clause

APP = wasm-runner

ARCH_LIBDIR ?= /lib/$(shell $(CC) -dumpmachine)

SELF_EXE = cache/$(APP)

.PHONY: all
all: $(APP).manifest
ifeq ($(SGX),1)
all: $(APP).manifest.sgx $(APP).sig
endif

ifeq ($(DEBUG),1)
GRAMINE_LOG_LEVEL = debug
else
GRAMINE_LOG_LEVEL = error
endif

# 削除証明にSGX quoteを含める場合は RA_TYPE=dcap を指定（要: ホスト側DCAPスタック）
RA_TYPE ?= none

# 注意: SGX/RA_TYPE を切り替えるときは `make clean` してからマニフェストを再生成すること
$(APP).manifest: $(APP).manifest.template
	gramine-manifest \
		-Dlog_level=$(GRAMINE_LOG_LEVEL) \
		-Darch_libdir=$(ARCH_LIBDIR) \
		-Dself_exe=$(SELF_EXE) \
		-Dsgx=$(SGX) \
		-Dra_type=$(RA_TYPE) \
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

.PHONY: run
run:
	@mkdir -p data_store
	$(GRAMINE) $(APP)

.PHONY: clean
clean:
	$(RM) -rf *.sig *.manifest.sgx *.manifest OUTPUT

.PHONY: distclean
distclean: clean
	$(RM) -rf cache/*
