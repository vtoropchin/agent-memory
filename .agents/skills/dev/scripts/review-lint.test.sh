#!/bin/sh
# Тест review-lint.sh: чистый REVIEW.md проходит, каждый вид нарушения ловится.
# Запуск: sh <каталог скила dev>/scripts/review-lint.test.sh
# shellcheck disable=SC2016 # обратные кавычки в sed — буквальный текст REVIEW.md

set -u
lint=$(cd "$(dirname "$0")" && pwd)/review-lint.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/review-lint-test.XXXXXX") || exit 2
trap 'rm -rf "$work"' EXIT HUP INT TERM
failed=0

repo=$work/repo
mkdir -p "$repo/docs" "$repo/src/auth" "$repo/migrations" "$repo/.claude/skills/dev"
git -C "$repo" init -q
printf 'x\n' >"$repo/docs/a.md"
printf 'x\n' >"$repo/src/auth/login.c"
printf 'x\n' >"$repo/migrations/001.sql"
printf 'x\n' >"$repo/.claude/skills/dev/SKILL.md"
printf 'REVIEW.md merge=union\n' >"$repo/.gitattributes"

clean() {
	cat <<'EOF'
# REVIEW.md

## Как здесь выглядит сбой

- пользователь не может войти

## Высокий риск

- `REVIEW.md`, `.gitattributes` — правила ревью
- `.claude/skills/dev/**` — фазы ревью
- `migrations/**` — миграции

## Безопасность

- `src/auth/**` — вход

## Низкий риск

- `docs/**` — документация

## Не ревьюить

- `build/**`

## Правила замечаний

- Не больше 3 Nit.

## Повторяющиеся находки

<!-- - [нет/такого/**] закомментированное правило не читается — 2026-01-01, пример -->
- [src/auth/**] сессия закрывается при любой ошибке входа — 2026-10-01, задача 01ABC
- [migrations/**] миграция обратима — 2026-10-02, коммит abc1234, до: 2027-01-01
EOF
}

# check <имя> <ожидаемый код> <подстрока вывода или пусто>; REVIEW.md — из stdin.
check() {
	cat >"$repo/REVIEW.md"
	out=$(sh "$lint" --today 2026-10-23 "$repo/REVIEW.md" 2>&1)
	code=$?
	if [ "$code" -ne "$2" ]; then
		echo "FAIL $1: код $code, ждали $2"; echo "$out"; : >"$work/failed"; return
	fi
	if [ -n "$3" ] && ! printf '%s\n' "$out" | grep -q -F "$3"; then
		echo "FAIL $1: нет «$3» в выводе"; echo "$out"; : >"$work/failed"; return
	fi
	echo "ok   $1"
}

clean | check "чистый файл" 0 "нарушений нет"
clean | sed '/^## Безопасность$/d' | check "нет заголовка" 1 "нет заголовка «## Безопасность»"
{ clean; printf '\n## Низкий риск\n'; } | check "повтор заголовка" 1 "повторяется 2 раз"
clean | sed 's|`docs/\*\*`|`nodocs/**`|' | check "мёртвый путь риска" 1 "путь \`nodocs/**\` не совпадает"
clean | sed 's|`REVIEW.md`, `.gitattributes`|`.gitattributes`|' | check "REVIEW.md не в высоком риске" 1 "покрывающего сам REVIEW.md"
clean | sed 's|`REVIEW.md`, `.gitattributes`|`REVIEW.md`|' | check ".gitattributes не в высоком риске" 1 "покрывающего .gitattributes"
clean | sed '/^- `.claude\/skills\/dev\/\*\*`/d' | check "dev не в высоком риске" 1 "нет пути \`.claude/skills/dev/**\`"
clean | sed 's|^- `docs/\*\*` — документация$|- `docs/**`, `.claude/skills/**` — документация|' |
	check "путь .claude/ в низком риске" 1 "путь \`.claude/skills/**\` под каталогом инструментов агента"
clean | sed 's|^- `docs/\*\*` — документация$|- `docs/**`, `**/SKILL.md` — документация|' |
	check "низкий риск захватывает .claude/" 1 "захватывает файлы каталога инструментов агента"
clean | sed 's|^- `build/\*\*`$|- `build/**`, `.claude/**`|' |
	check "путь .claude/ в «Не ревьюить»" 1 "Не ревьюить: путь \`.claude/**\` под каталогом инструментов агента"
clean | sed 's|^- `build/\*\*`$|- `build/**`, `**/SKILL.md`|' |
	check "«Не ревьюить» захватывает .claude/" 1 "Не ревьюить: путь \`**/SKILL.md\` захватывает"
mkdir -p "$repo/shared/tools" && printf 'x\n' >"$repo/shared/tools/run.sh"
ln -s ../shared/tools "$repo/.claude/tools"
clean | sed 's|^- `build/\*\*`$|- `build/**`, `shared/**`|' |
	check "«Не ревьюить» захватывает цель симлинка из .claude/" 1 "цели его симлинка"
rm "$repo/.claude/tools"
clean | sed 's|^- `migrations/\*\*` — миграции$|- миграции схемы данных:\
  `migrations/**` — откат дорогой|' | check "живой путь на строке-продолжении" 0 "нарушений нет"
clean | sed 's|^- `migrations/\*\*` — миграции$|- миграции схемы данных:\
  `migrations/**`, `nomigrations/**` — откат дорогой|' |
	check "мёртвый путь на строке-продолжении" 1 "путь \`nomigrations/**\` не совпадает"
clean | sed 's|^- \[src/auth/\*\*\] |- |' | check "правило без якоря" 1 "правило без якоря"
clean | sed 's|\[src/auth/\*\*\]|[src/payments/**]|' | check "мёртвый якорь" 1 "якорь [src/payments/**] не совпадает"
clean | sed 's| — 2026-10-01, задача 01ABC||' | check "правило не по формату" 1 "не по формату"
clean | sed 's|до: 2027-01-01|до: 2026-10-22|' | check "истёкший срок" 1 "срок до: 2026-10-22 истёк"
{
	clean
	i=1
	while [ $i -le 15 ]; do
		echo "- [docs/**] правило номер $i — 2026-10-03, задача 0$i"; i=$((i + 1))
	done
} | check "больше 15 правил" 1 "правил 17, потолок 15"
{
	clean
	i=1
	while [ $i -le 120 ]; do echo; i=$((i + 1)); done
} | check "больше 150 строк" 1 "предел 150"
{ clean; echo "- [migrations/**] миграция обратима — 2026-10-05, задача 02XYZ"; } |
	check "дубль правила" 1 "правило повторяется дословно: миграция обратима"
{ clean | sed 's|^- `docs/\*\*` — документация$|- `docs/**` — документация\
- `docs/**` — документация|'; } | check "дубль пункта" 1 "пункт повторяется дословно"
# Набор установлен в .agents/skills (DeepSeek Harness, Hermes и др.), а не в .claude/.
mkdir -p "$repo/.agents/skills" && mv "$repo/.claude/skills/dev" "$repo/.agents/skills/dev"
clean | sed 's|`.claude/skills/dev/\*\*`|`.agents/skills/dev/**`|' | check "dev в .agents/skills" 0 "нарушений нет"
clean | check "dev в .agents/skills, в высоком риске старый путь" 1 "нет пути \`.agents/skills/dev/**\`"
clean | sed 's|`.claude/skills/dev/\*\*`|`.agents/skills/dev/**`|; s|^- `build/\*\*`$|- `build/**`, `.agents/**`|' |
	check "путь .agents/ в «Не ревьюить»" 1 "Не ревьюить: путь \`.agents/**\` под каталогом инструментов агента"
mkdir -p "$repo/.hermes/skills" && printf 'x\n' >"$repo/.hermes/skills/x.md"
clean | sed 's|`.claude/skills/dev/\*\*`|`.agents/skills/dev/**`|; s|^- `docs/\*\*` — документация$|- `docs/**`, `.hermes/**` — документация|' |
	check "скрытый каталог клиента со skills/ в низком риске" 1 "под каталогом инструментов агента"
rm -rf "$repo/.hermes"
mv "$repo/.agents/skills/dev" "$repo/.claude/skills/dev"
printf 'other merge=union\n' >"$repo/.gitattributes"
clean | check "нет merge=union" 1 "REVIEW.md merge=union"
printf 'REVIEW.md merge=union\n' >"$repo/.gitattributes"

out=$(sh "$lint" --help); code=$?
if [ "$code" -eq 0 ] && printf '%s\n' "$out" | grep -q 'Использование'; then echo "ok   --help"; else echo "FAIL --help"; failed=1; fi
sh "$lint" "$work/нет-файла.md" >/dev/null 2>&1
if [ $? -eq 2 ]; then echo "ok   нет файла → код 2"; else echo "FAIL нет файла"; failed=1; fi

# check работает в конвейере (подоболочке): провал отмечается файлом, а не переменной.
[ -e "$work/failed" ] && failed=1
[ "$failed" -eq 0 ] && echo "review-lint.test: все проверки прошли"
exit "$failed"
