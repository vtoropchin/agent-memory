.PHONY: build test run import install install-user install-system uninstall-system bin/memoryd

# mattn/go-sqlite3 требует CGO и тег sqlite_fts5 для поддержки полнотекстового поиска FTS5.

# PREFIX — куда install-system ставит бинарь. Дефолт /usr/local (bin на PATH у всех
# пользователей). Переопределяется: make install-system PREFIX=~/.local
PREFIX ?= /usr/local
# Рекурсивное присваивание (=), а не := — чтобы target-специфичный PREFIX
# (напр. в install-user) корректно доходил до BINDIR.
BINDIR = $(PREFIX)/bin
# SUDO — повышение прав для записи в системный каталог (/usr/local/bin принадлежит
# root). Для пользовательского PREFIX (напр. ~/.local, ~/bin) отключается пустым
# значением: make install-system PREFIX=~/.local SUDO=
SUDO ?= sudo

build:
	CGO_ENABLED=1 go build -tags sqlite_fts5 ./...

# install кладёт memoryd на PATH через go install (в $GOBIN/$GOPATH/bin) — удобно
# для Go-разработчиков, если $GOPATH/bin уже на PATH.
install:
	CGO_ENABLED=1 go install -tags sqlite_fts5 ./cmd/memoryd

# install-system компилирует memoryd и ставит его глобально в систему ($(BINDIR),
# по умолчанию /usr/local/bin — обычно на PATH у всех). Так его находит .mcp.json,
# сгенерированный `memoryd init` с фолбэком-командой "memoryd". Сборка (bin/memoryd)
# идёт от текущего пользователя, а $(SUDO) поднимает права только на шаги записи в
# систему — запусти обычным `make install-system` (спросит пароль на копирование),
# НЕ `sudo make ...`, иначе go build выполнится под root. mkdir -p не трогает права
# уже существующего /usr/local/bin (в отличие от install -d, который его chmod-ит).
install-system: bin/memoryd
	$(SUDO) mkdir -p "$(BINDIR)"
	$(SUDO) install -m 0755 bin/memoryd "$(BINDIR)/memoryd"
	@echo "установлено: $(BINDIR)/memoryd"

# install-user ставит memoryd ТОЛЬКО для текущего пользователя, БЕЗ sudo, в
# ~/.local/bin (переопределяет PREFIX и обнуляет SUDO для install-system).
# Для самого MCP-сервера PATH не нужен — .mcp.json хранит абсолютный путь бинаря;
# PATH нужен лишь чтобы звать `memoryd` короткой командой, поэтому в конце —
# подсказка, если ~/.local/bin ещё не на PATH.
install-user: PREFIX := $(HOME)/.local
install-user: SUDO :=
install-user: install-system
	@case ":$$PATH:" in \
	  *":$(HOME)/.local/bin:"*) echo "$(HOME)/.local/bin уже на PATH — можно звать: memoryd" ;; \
	  *) echo "добавь в ~/.zshrc: export PATH=\"$$HOME/.local/bin:$$PATH\"  (иначе бинарь ставится, но команда memoryd не найдётся)" ;; \
	esac

# uninstall-system удаляет бинарь, поставленный install-system.
uninstall-system:
	$(SUDO) rm -f "$(BINDIR)/memoryd"
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

# import переносит файловую память (.claude/memory/) в SQLite-хранилище memoryd
# (DESIGN.md §14 Этап 5). Доп. флаги (-dir/-db/-scope/-dry-run) — напрямую через
# go run, напр.: CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport -dry-run
import:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memimport
