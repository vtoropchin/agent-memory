# Адаптер: DeepSeek Harness (dsh)

Читайте этот файл, только если текущий клиент — DeepSeek Harness. Общая модель
возможностей — в [runtime.md](runtime.md), выбор модели — в [model-selection.md](model-selection.md).
Имена ниже относятся к dsh, а не к стандарту Agent Skills; сверяйте их с фактическими
схемами инструментов сессии: текст скила не создаёт возможностей.

## Транспорт

`delegated`: субагент в режиме fresh context (не fork беседы) с возвратом итога.
MCP memo подключается в профиле/patch-слое dsh; проверьте, что child его видит.

## Модели child

| Что | Как в dsh |
|---|---|
| Источник каталога | Только `list_subagent_models`. Он возвращает пересечение опубликованного каталога dsh и маршрутов, разрешённых **этой сессии**. Селектор модели parent и опубликованный каталог источником не являются |
| Одна модель в списке | Совпадает с моделью parent — выбор модели субагентов выключен, child идёт маршрутом parent: `selection: inherit` (проверено: parent на Pro — в списке только Pro, parent на Flash — только Flash). Отличается от модели parent — выбор включён с одной моделью: `single_candidate`. Модель parent неизвестна — `single_candidate` с подсказкой ниже. Порог проверяется во всех случаях |
| Признак запрета | Запрос модели вне политики отвечает ошибкой вида `child LLM route "<provider>/<model>" is not allowed for this Session` — модель есть в dsh, но child этой сессии её не получит |
| Модель parent | Пока выбор модели субагентов выключен, все child идут на модели parent: дешёвая модель parent удешевляет child, но ослабляет координатора. Правильный путь — включить выбор (ниже), а не понижать parent. Разрешённые child модели задаёт только настройка, не модель parent |

## Уровень рассуждений child

«Уровень рассуждений» dsh — это уровень усилия из [model-selection.md](model-selection.md).
Проверено в dsh (сверяйте с фактической схемой сессии — версии меняются):

| Что | Как в dsh |
|---|---|
| Запуск субагента | Инструмент `subagent`: `description`, `prompt` (обязательные), `cwd`, `provider`, `model`, `reasoning_effort` |
| Значения уровня | `list_subagent_models` с `provider` и `model` перечисляет «Reasoning efforts» модели. У `deepseek-v4-pro`: `off` < `low` < `high` (по умолчанию) < `max` |
| Каталог | `effort_values` — эти значения по возрастанию, `effort_selection: chosen`. Нет `reasoning_effort` в схеме или значений у модели — `effort_selection: inherit` |

Сопоставление для `off, low, high, max` (n = 4): `low` → `off`, `medium` → `low`,
`high` → `high`, `max` → `max`. `off` выключает рассуждение: так идут scout и read-only
worker — это самый дешёвый уровень, а ошибку в фактах scout ловит planner, который проверяет
его ссылки.
Уровень можно выбирать и при `selection: inherit`: это единственный рычаг экономии, пока
выбор модели субагентов не включён. Поэтому в dsh с `reasoning_effort` в схеме
`effort_selection: chosen`, и при Jev не `off` `jev.py calibrate` нужен любому назначению,
кроме scout, при любом `selection`. Не передали
`reasoning_effort` — child идёт на умолчании модели (у `deepseek-v4-pro` — `high`), то есть
scout и read-only worker на модели parent работают на полном рассуждении: передавайте
значение явно в каждом вызове `subagent`.

### Как включить выбор модели субагентов

Это настройка пользователя в приложении dsh, а не действие скила. При `selection: inherit`
один раз сообщите пользователю одной строкой:

1. Settings → Subagent → Model selection.
2. Включить «Allow agents to choose models for Subagents».
3. В «Models agents may choose» отметить модели всех нужных классов — минимум одну
   дешёвую (`light`) и одну сильную для кода (`coding`); сохранить.
4. Настройка действует только для **новых** сессий: текущую нужно перезапустить.

До перезапуска работайте с `selection: inherit`: все child на модели parent, в отчёте —
«принудительно, выбора нет». Калибровку Jev (`jev.py calibrate`) вызывайте только ради
уровня рассуждений (`effort_selection: chosen`); нет и его — не вызывайте.
