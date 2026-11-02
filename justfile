set shell := ["bash", "-uc"]

default:
	@just --list

# Run the mock: SMPP on :2775, control API on :8080
run accounts="pee:secret":
	SMSC_ACCOUNTS={{accounts}} go run ./cmd

test:
	go test -race -count=1 ./...

ci:
	test -z "$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...
	golangci-lint run ./...
