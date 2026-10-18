VERSION ?= 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test clean release

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o buggy .

install:
	go install -trimpath -ldflags "$(LDFLAGS)" .

test:
	go vet ./...
	go test ./...

release:
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/buggy-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/buggy-linux-arm64 .
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/buggy-darwin-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/buggy-darwin-amd64 .
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/buggy-windows-amd64.exe .

clean:
	rm -rf buggy dist
