GO ?= go

.PHONY: all build test race fuzz-short bench demo check fmt vet cross clean

all: build

build:
	$(GO) build -o bin/ ./cmd/lantern

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

fuzz-short:
	$(GO) test -fuzz=FuzzAnalyze -fuzztime=20s ./internal/analysis
	$(GO) test -fuzz=FuzzCodecRoundtrip -fuzztime=20s ./internal/codec
	$(GO) test -fuzz=FuzzLexer -fuzztime=20s ./internal/query

bench:
	$(GO) test -bench=. -benchmem -run=NONE -benchtime=1x ./internal/index ./internal/query

demo: build
	bash scripts/demo.sh

corpus:
	$(GO) run ./scripts/gen_corpus -dir corpus -n 100000 -target-mb 500

bench-latency: build
	$(GO) run ./scripts/bench_search -index .lantern -mode and

fmt:
	gofmt -l .

vet:
	$(GO) vet ./...

cross:
	GOOS=windows $(GO) build ./...
	GOOS=darwin $(GO) build ./...
	GOOS=linux $(GO) build ./...

clean:
	rm -rf bin
