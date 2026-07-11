.PHONY: build test run import

# mattn/go-sqlite3 требует CGO и тег sqlite_fts5 для поддержки полнотекстового поиска FTS5.

build:
	CGO_ENABLED=1 go build -tags sqlite_fts5 ./...

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

run:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd

# import переносит файловую память (.claude/memory/) в SQLite-хранилище memoryd
# (DESIGN.md §14 Этап 5). Доп. флаги (-dir/-db/-scope/-dry-run) — напрямую через
# go run, напр.: CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport -dry-run
import:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport
