.PHONY: test fmt-check image

test:
	go vet ./...
	go test -race ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

image:
	docker build -t review-sieve:dev .
