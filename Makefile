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
	$(GO) test -bench=. -benchmem -run=NONE -benchtime=1x ./...

demo: build
	bash scripts/demo.sh

check: build
	./bin/lantern check || true

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
