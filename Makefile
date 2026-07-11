.PHONY: build test run web import install uninstall bin/memoryd

# mattn/go-sqlite3 требует CGO и тег sqlite_fts5 для поддержки полнотекстового поиска FTS5.

# PREFIX — куда install ставит бинарь (для текущего пользователя, без sudo). Дефолт
# ~/.local (bin обычно на PATH). Переопределяется: make install PREFIX=~/some/dir
PREFIX ?= $(HOME)/.local
BINDIR = $(PREFIX)/bin

build:
	CGO_ENABLED=1 go build -tags sqlite_fts5 ./...

# install собирает memoryd и ставит его для текущего пользователя в $(BINDIR)
# (по умолчанию ~/.local/bin), БЕЗ sudo. Так его находит .mcp.json, сгенерированный
# `memoryd init` (фолбэк-команда "memoryd"). Для самого MCP-сервера PATH не нужен —
# .mcp.json хранит абсолютный путь бинаря; PATH нужен лишь чтобы звать `memoryd`
# короткой командой, поэтому в конце — подсказка, если $(BINDIR) ещё не на PATH.
install: bin/memoryd
	mkdir -p "$(BINDIR)"
	install -m 0755 bin/memoryd "$(BINDIR)/memoryd"
	@echo "установлено: $(BINDIR)/memoryd"
	@case ":$$PATH:" in \
	  *":$(BINDIR):"*) ;; \
	  *) echo "добавь в ~/.zshrc: export PATH=\"$(BINDIR):$$PATH\"  (иначе команда memoryd не найдётся)" ;; \
	esac

# uninstall удаляет установленный install бинарь.
uninstall:
	rm -f "$(BINDIR)/memoryd"
	@echo "удалено: $(BINDIR)/memoryd"

# bin/memoryd — бинарь для догфудинга (DESIGN.md §14 Этап 5): его запускает
# Claude Code как MCP-сервер `agentmemory` из .mcp.json. Цель .PHONY — всегда
# пересобирает свежий бинарь. Каталог bin/ игнорируется git.
bin/memoryd:
	CGO_ENABLED=1 go build -tags sqlite_fts5 -o bin/memoryd ./cmd/memoryd

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

run:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd

# web поднимает read-only HTTP-морду просмотра памяти (DESIGN.md §12, Этап 6,
# задача 6.2) на 127.0.0.1:7070. Доп. флаги (-addr/-config/-db) — напрямую через
# go run, напр.: CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd web -db ./memory.db
web:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd web

# import переносит файловую память (.claude/memory/) в SQLite-хранилище memoryd
# (DESIGN.md §14 Этап 5). Доп. флаги (-dir/-db/-scope/-dry-run) — напрямую через
# go run, напр.: CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport -dry-run
import:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport
