BIN := bin/go-dart

.PHONY: build test lint run fmt clean

build:
	go build -o $(BIN) ./cmd/go-dart

test:
	go test ./...

lint:
	gofmt -l . | (! grep .) && go vet ./...

fmt:
	gofmt -w .

run: build
	$(BIN) -c configs/go-dart.yml serve

clean:
	rm -rf bin work go-dart.db*
