BIN := bin/manent

.PHONY: build test lint run fmt clean

build:
	go build -o $(BIN) ./cmd/manent

test:
	go test ./...

lint:
	gofmt -l . | (! grep .) && go vet ./...

fmt:
	gofmt -w .

run: build
	$(BIN) -c configs/manent.yml serve

clean:
	rm -rf bin work manent.db*
