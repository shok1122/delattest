APP = wasm-runner

.PHONY: builder
builder:
	docker build --target builder -t wasm-runner-builder .

.PHONY: runner
runner:
	docker build --target runtime -t wasm-runner-runtime .

.PHONY: run
run:
	docker run -d --rm \
		-p 3000:3000 \
		--name wasm-runner \
		wasm-runner-runtime

.PHONY: stop
stop:
	docker stop wasm-runner

.PHONY: extract
extract: builder
	docker create --name tmp wasm-runner-builder
	docker cp tmp:/build/bin/$(APP) ./cache/$(APP)
	docker rm tmp
	sudo chown $(shell whoami):$(shell whoami) ./cache/$(APP)

.PHONY: install
install: extract
	cp ./cache/$(APP) ../cache/$(APP)

.PHONY: upload
upload: extract
	scp ./cache/$(APP) delattest:~/dev/delattest/cache/$(APP)

.PHONY: gen-go-sum
gen-go-sum:
	docker run --rm -v .:/work -w /work golang:1.25-bookworm go mod tidy
