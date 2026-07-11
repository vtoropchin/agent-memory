.PHONY: build test run

# mattn/go-sqlite3 требует CGO и тег sqlite_fts5 для поддержки полнотекстового поиска FTS5.

build:
	CGO_ENABLED=1 go build -tags sqlite_fts5 ./...

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

run:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd
