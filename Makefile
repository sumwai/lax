.PHONY: build test vet fmt check clean

build:
	go build -o switchboard ./cmd/switchboard

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: vet test build

clean:
	rm -f switchboard
