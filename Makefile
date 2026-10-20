.PHONY: generate run test

generate:
	go run github.com/a-h/templ/cmd/templ@v0.3.960 generate

run: generate
	go run ./cmd/web

test: generate
	go test ./...
