.PHONY: all
all:
	docker run --rm \
		-v $$PWD:/work \
		wasi-build \
		clang \
			--sysroot=/opt/wasi-sdk/share/wasi-sysroot \
			--target=wasm32-wasi \
			-O2 \
			-o app.wasm main.c


