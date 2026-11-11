# Происхождение пакета

Источник: https://github.com/ayghri/i-have-adhd, коммит
`1ecea1993b7fce257ffbd686c070948b5e07eda6` (2026-10-11), файл `skills/i-have-adhd/SKILL.md`.
Лицензия MIT, копия — `LICENSE` в корне пакета; авторство сохранено.

## Что изменено относительно оригинала

- Убрано поле `disable-model-invocation: true`: его нет в спецификации Agent Skills, и
  валидатор skills-ref его отклоняет. Требование «включать только явно» перенесено в
  `description`. Жёсткого запрета автоматического вызова больше нет.
- В `description` добавлено включение по умолчанию оркестратором (`dev`, `orchestrate`).
- Добавлены разделы «Scope: what the rules shape» и «Turned on by an orchestrator»:
  правила формируют только сообщения человеку, а не брифы, отчёты агентов, память, код
  и файлы с заданным форматом.
- В `metadata` добавлены `upstream` и `upstream-commit`.
- Остальной текст правил не менялся.

## Что не перенесено

- Хуки SessionStart (`hooks/`) и флаг `~/.claude/.i-have-adhd-always`: это механизм одного
  клиента вне границ пакета. Режим «по умолчанию» здесь задают оркестраторы набора.
- Манифесты плагинов и расширений (`.claude-plugin/`, `gemini-extension.json`,
  `qwen-extension.json`, `kimi.plugin.json`, `extensions/`) и адаптеры
  `agents/gemini.toml`, `agents/openai.yaml`: пакет ставится стандартным skills CLI.
- Скрипты оценки (`scripts/`): они требуют API моделей и не нужны для работы скила.

## Как обновить

1. Сравнить `skills/i-have-adhd/SKILL.md` нового коммита upstream с закреплённым.
2. Перенести изменения правил, сохранив изменения из списка выше.
3. Обновить `upstream-commit` в `metadata` и этот файл, прогнать проверки репозитория.
