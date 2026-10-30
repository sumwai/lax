.PHONY: build test vet fmt check clean

build:
	go build -o lax ./cmd/lax

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: vet test build

clean:
	rm -f lax
