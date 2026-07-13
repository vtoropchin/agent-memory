.PHONY: build test run web web-css import install uninstall bin/memo

# mattn/go-sqlite3 требует CGO и тег sqlite_fts5 для поддержки полнотекстового поиска FTS5.

# PREFIX — куда install ставит бинарь (для текущего пользователя, без sudo). Дефолт
# ~/.local (bin обычно на PATH). Переопределяется: make install PREFIX=~/some/dir
PREFIX ?= $(HOME)/.local
BINDIR = $(PREFIX)/bin

build:
	CGO_ENABLED=1 go build -tags sqlite_fts5 ./...

# install собирает memo и ставит его для текущего пользователя в $(BINDIR)
# (по умолчанию ~/.local/bin), БЕЗ sudo. Так его находит .mcp.json, сгенерированный
# `memo init` (фолбэк-команда "memo"). Для самого MCP-сервера PATH не нужен —
# .mcp.json хранит абсолютный путь бинаря; PATH нужен лишь чтобы звать `memo`
# короткой командой, поэтому в конце — подсказка, если $(BINDIR) ещё не на PATH.
install: bin/memo
	mkdir -p "$(BINDIR)"
	install -m 0755 bin/memo "$(BINDIR)/memo"
	@echo "установлено: $(BINDIR)/memo"
	@case ":$$PATH:" in \
	  *":$(BINDIR):"*) ;; \
	  *) echo "добавь в ~/.zshrc: export PATH=\"$(BINDIR):$$PATH\"  (иначе команда memo не найдётся)" ;; \
	esac

# uninstall удаляет установленный install бинарь.
uninstall:
	rm -f "$(BINDIR)/memo"
	@echo "удалено: $(BINDIR)/memo"

# bin/memo — бинарь для догфудинга (DESIGN.md §14 Этап 5): его запускает
# Claude Code как MCP-сервер `memo` из .mcp.json. Цель .PHONY — всегда
# пересобирает свежий бинарь. Каталог bin/ игнорируется git.
bin/memo:
	CGO_ENABLED=1 go build -tags sqlite_fts5 -o bin/memo ./cmd/memo

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

run:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memo

# web поднимает read-only HTTP-морду просмотра памяти (DESIGN.md §12, Этап 6,
# задача 6.2) на 127.0.0.1:7070. Доп. флаги (-addr/-config/-db) — напрямую через
# go run, напр.: CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memo web -db ./memory.db
web:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memo web

# web-css собирает cmd/memo/web/tailwind.css из cmd/memo/tailwind.input.css
# standalone-бинарём Tailwind CLI (задача 6.3, DESIGN.md §12, Этап 6) — без Node/npm.
# Tailwind встраивается в бинарь memo (не CDN): результат коммитится в git и
# раздаётся как обычная статика вместе с index.html/app.js. Бинарь Tailwind CLI
# качается один раз в bin/tailwindcss (bin/ в .gitignore, цель идемпотентна).
TAILWIND_VERSION = v4.3.2
TAILWIND_BIN = bin/tailwindcss

web-css:
	@os_arch="$$(uname -s)-$$(uname -m)"; \
	case "$$os_arch" in \
	  Darwin-arm64) asset=macos-arm64 ;; \
	  Darwin-x86_64) asset=macos-x64 ;; \
	  Linux-aarch64|Linux-arm64) asset=linux-arm64 ;; \
	  Linux-x86_64) asset=linux-x64 ;; \
	  *) echo "неподдерживаемая ОС/архитектура для standalone Tailwind CLI: $$os_arch"; exit 1 ;; \
	esac; \
	if [ -x "$(TAILWIND_BIN)" ]; then \
	  echo "Tailwind CLI уже скачан: $(TAILWIND_BIN)"; \
	else \
	  echo "качаю Tailwind CLI $(TAILWIND_VERSION) ($$asset)..."; \
	  mkdir -p bin; \
	  curl -fL -o "$(TAILWIND_BIN)" "https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$$asset"; \
	  chmod +x "$(TAILWIND_BIN)"; \
	fi
	@echo "собираю cmd/memo/web/tailwind.css..."
	"$(TAILWIND_BIN)" -i cmd/memo/tailwind.input.css -o cmd/memo/web/tailwind.css --minify
	@echo "готово: cmd/memo/web/tailwind.css"

# import переносит файловую память (.claude/memory/) в SQLite-хранилище memo
# (DESIGN.md §14 Этап 5). Доп. флаги (-dir/-db/-scope/-dry-run) — напрямую через
# go run, напр.: CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport -dry-run
import:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport
