.PHONY: build test run import bin/memoryd

# mattn/go-sqlite3 требует CGO и тег sqlite_fts5 для поддержки полнотекстового поиска FTS5.

build:
	CGO_ENABLED=1 go build -tags sqlite_fts5 ./...

# bin/memoryd — бинарь для догфудинга (DESIGN.md §14 Этап 5): его запускает
# Claude Code как MCP-сервер `agentmemory` из .mcp.json. Цель .PHONY — всегда
# пересобирает свежий бинарь. Каталог bin/ игнорируется git.
bin/memoryd:
	CGO_ENABLED=1 go build -tags sqlite_fts5 -o bin/memoryd ./cmd/memoryd

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

run:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd

# import переносит файловую память (.claude/memory/) в SQLite-хранилище memoryd
# (DESIGN.md §14 Этап 5). Доп. флаги (-dir/-db/-scope/-dry-run) — напрямую через
# go run, напр.: CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport -dry-run
import:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport
