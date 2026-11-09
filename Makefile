GO ?= go

.PHONY: all build test race check fmt fmt-check vet cross fuzz-short bench bench-latency corpus ui-check docs-check demo clean

all: build

build:
	$(GO) build -o bin/lantern ./cmd/lantern

test:
	$(GO) test ./... -timeout 20m

race:
	$(GO) test -race ./... -timeout 25m

# Local gate: everything the Linux CI enforces, minus the race detector
# (which needs a C compiler) and the fuzz targets (which are time-boxed).
check: fmt-check vet test ui-check docs-check

fmt:
	$(GO) fmt ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

cross:
	CGO_ENABLED=0 GOOS=windows $(GO) build ./...
	CGO_ENABLED=0 GOOS=darwin  $(GO) build ./...
	CGO_ENABLED=0 GOOS=linux   $(GO) build ./...

fuzz-short:
	$(GO) test -run=FuzzAnalyze -fuzz=FuzzAnalyze -fuzztime=20s ./internal/analysis
	$(GO) test -run=FuzzCodecRoundtrip -fuzz=FuzzCodecRoundtrip -fuzztime=20s ./internal/codec
	$(GO) test -run=FuzzLexer -fuzz=FuzzLexer -fuzztime=20s ./internal/query

bench:
	$(GO) test -bench=. -benchmem -run=NONE -benchtime=1x ./internal/index ./internal/query

bench-latency: build
	$(GO) run ./scripts/bench_search -index .lantern -mode and

corpus:
	$(GO) run ./scripts/gen_corpus -dir corpus -n 100000 -target-mb 500

# The embedded UI must stay offline-capable and bilingual.
ui-check:
	@if grep -Eq 'https?://|cdn\.|unpkg|jsdelivr|fonts\.googleapis' internal/server/ui/index.html; then \
		echo "UI references an external resource"; exit 1; \
	fi
	@for k in '"en"' '"zh"'; do \
		grep -q $$k internal/server/ui/index.html || { echo "missing UI locale $$k"; exit 1; }; \
	done
	@echo "UI: offline + en/zh OK"

# Both README files must exist and reference their own language screenshot.
docs-check:
	@test -f README.md && test -f README.zh-CN.md
	@test -f docs/screenshots/ui-en.png && test -f docs/screenshots/ui-zh.png
	@grep -q 'docs/screenshots/ui-en.png' README.md
	@grep -q 'docs/screenshots/ui-zh.png' README.zh-CN.md
	@echo "docs: README + screenshots OK"

demo: build
	bash scripts/demo.sh

clean:
	rm -rf bin .tmp
