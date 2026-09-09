.PHONY: build install test vet clean

build:
	mkdir -p bin
	go build -o ./bin/ ./cmd/...

install:
	go install ./cmd/...

test:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -rf bin
