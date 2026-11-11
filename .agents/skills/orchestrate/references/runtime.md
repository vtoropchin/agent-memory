# Runtime: клиент агента и транспорт v3

Читайте перед первым запуском child, отправкой сообщений, ожиданием и восстановлением.
Протокол v3 в [contracts.md](contracts.md) не зависит от клиента. Этот файл отображает его
на фактические инструменты текущего кодинг-агента: Claude Code, DeepSeek Harness, Hermes
Agent или другого клиента с поддержкой Agent Skills. Текст скила не создаёт возможностей
и не отменяет ограничения клиента: опирайтесь на схемы инструментов, реально доступные
в текущей сессии, а не на имена из памяти или из этого файла.

## Возможности

| Код | Возможность | Что подтвердить по фактической схеме |
|---|---|---|
| SPAWN | Запуск child-агента со свежим контекстом и самодостаточным prompt в том же проекте | Fresh context, а не fork беседы; проект/cwd; разрешённый выбор модели; child подчинён правам клиента |
| CHANNEL | Адресуемая двусторонняя связь parent↔child, пока child работает | Реальный адрес, подлинный источник, статус доставки |
| RECONCILE | Сверка своих запусков по ID: список, состояние, транскрипт | Фактические IDs и состояния связанных запусков |
| WAKE | Push-уведомление о сообщении или завершении child | Событие самого клиента, не опрос |
| RESULT | Возврат итогового сообщения child как результата вызова parent | Итог связан с конкретным вызовом; разговор child завершён при возврате |
| MEMO | MCP memo у parent и, по возможности, у child | Схемы `memory_*`; наследует ли child MCP-серверы parent |
| JUDGE | Необязательно: parent запускает `python3` (≥ 3.9) в shell и достаёт до хоста Jev | `jev.py config` и `jev.py check` ([jev](jev.md)); нет — `jev.mode: off`, это не блокер |
| WORKTREE | Отдельный Git worktree у каждого writer | Изоляция клиента (worktree-режим субагента/сессии) либо child может работать в каталоге, который parent создал `git worktree add`; child видит свой git-dir и общий common-dir |

SPAWN — механизм самого клиента-хоста, видимый и подчинённый его правам. Процессы, запущенные
в обход клиента через shell (CLI другого агента, tmux, SDK-скрипты), новый MCP или внешний API,
не заменяют child: они обходят видимость, права и сверку. Fork всей беседы parent не даёт
независимости verifier и переносит контекст parent, поэтому для v3 нужен fresh-вариант.

## Выбор транспорта

| `transport` | Нужны | Когда |
|---|---|---|
| `session` | SPAWN, CHANNEL, RECONCILE; WAKE желателен | Клиент даёт долгоживущие адресуемые сессии (например, Claude Code Desktop) |
| `delegated` | SPAWN, RESULT | Клиент даёт субагентов со свежим контекстом, которые возвращают итог parent (например, DeepSeek Harness, Hermes Agent, Claude Code без инструментов сессий) |

1. Есть всё для `session` — используйте `session`. Переход на `delegated` при доступном
   `session` требует решения пользователя: так сохраняется прежнее поведение клиентов с сессиями.
2. Иначе есть SPAWN и RESULT — используйте `delegated`.
3. Иначе — `blocked: no_child_runtime`. Один агент не играет все роли в своём контексте:
   независимой проверки так не получить. Человек не становится каналом между ролями.

Сохраните в control до первого intent:

```yaml
runtime:
  client: <имя клиента, как его сообщает среда, либо unknown>
  transport: session # delegated
  capabilities: {spawn: true, channel: true, reconcile: true, wake: true, result: false, child_memo: true, worktree: client} # worktree: client | parent | none
  mapping: {spawn: <фактический tool>, channel: <tool|null>, reconcile: <tool|null>, wake: <механизм|null>, result: <tool|null>, worktree: <параметр изоляции клиента | git worktree add | null>}
  limits: [] # например: нет вложенной делегации, нет вопросов человеку у child, предел параллельных субагентов
  model_catalog: {client: <клиент>, source: <откуда>, models: [], selection: <chosen|single_candidate|inherit>, effort_values: [] , effort_selection: <chosen|inherit>} # см. model-selection.md, «Каталог моделей клиента»
  jev: {mode: off, provider: null, host: null, consent_ref: null} # off | shadow | apply; см. jev.md
```

WORKTREE выбирает Git policy ([параллельная доставка](parallel-delivery.md#4-worktree-и-ветки)):
`client` или `parent` — `worktree-integration/v1`; `none` — запасная линейная policy с
последовательной записью. Изоляция клиента предпочтительнее: клиент сам знает, где child
работает. Если клиент создаёт worktree от своей точки (например, от HEAD parent), child
переключается на ветку от `base_oid`, как сказано в контракте. Worktree, который клиент
удаляет сам при отсутствии изменений, не мешает: результат хранится в ветке задачи.

Transport фиксируется для каждого назначения. Resume в другом клиенте может выбрать новый
transport для новых назначений; незавершённые назначения сверяются по своему сохранённому mapping.

## Транспорт `session`

Поток v3 без изменений: intent → start → bind → `ready` → grant → работа → summary/release.
Адресная связь, сверка запусков по token и ожидание push-событий описаны в контракте.
Конкретное отображение для Claude Code Desktop — [runtime-claude-code-desktop.md](runtime-claude-code-desktop.md).
Для другого клиента с сессиями заполните `mapping` по его схемам по тем же правилам.

## Транспорт `delegated`

Субагент работает, пока parent ждёт результат вызова, и не получает сообщений по ходу работы.
Протокол v3 сохраняется, меняется только доставка:

1. **Identity.** `sender_session_id`/`author_session_id` — ID, который вернул клиент для вызова.
   Если клиент ID не возвращает, parent до вызова записывает в control handle
   `<assignment_id>/<gate|exec>` и использует его. Подлинность источника даёт то, что итог —
   результат собственного вызова parent; поле sender в тексте ничего не доказывает.
2. **Gate-вызов вместо ожидания grant.** Intent/token сохраняются до вызова. Brief содержит
   `transport: delegated`, `delegated_phase: gate`, `start_gate: await_grant`. Child читает
   brief и назначенные инструкции, ничего не выполняет и возвращает итог — envelope `ready`
   (или `question`/`blocked`).
3. **Grant.** Parent проверяет ready, записывает grant в control и подтверждает get.
4. **Exec-вызов.** Новый вызов того же assignment/attempt: brief (или его точный ref),
   ref на ready и полный envelope `grant`, `delegated_phase: exec`. Child сверяет grant
   с brief и, при наличии memo, с control через get; затем выполняет работу и возвращает
   итог — coordinator-summary или receipt с release. `read-grant` и `git-grant` идут так же.
5. **Слоты.** Gate и exec занимают слот; между ними слот остаётся зарезервирован.
   Независимые вызовы отправляйте **одновременно** (в одном ходе), если клиент выполняет их
   параллельно, в пределах `active_limit`; последовательный запуск независимого — потеря
   времени. Gate и exec одного назначения разделены, но gate разных назначений — нет.
6. **Вопрос во время exec.** CHANNEL нет, поэтому `question`/`blocked` завершают вызов итогом
   с остановленными процессами и manifest частичных изменений. Если записей и side effects
   не было, parent может повторить gate/exec того же назначения с уточнением в `decisions`.
   Иначе — continuation: новое назначение с `continuation_of`, тем же attempt и ответом в
   `decisions`; это не repair (контракт, «Транспорт»).
7. **Release.** Возврат вызова завершает разговор child, но не обязательно его фоновые процессы.
   Child останавливает их до итога и указывает `processes_finished`. Ошибка или timeout вызова —
   uncertain: writer остаётся владельцем, повтор не выполняется, пока inventory, транскрипт
   (если есть RECONCILE) и остаточные эффекты не сверены.
8. **Ожидание.** Вызов блокирует parent, WAKE не нужен. Если клиент запускает субагентов
   в фоне и сам уведомляет о завершении, это WAKE: используйте его, а не опрос.
9. **Размер итога.** Клиент обычно вставляет итог child целиком в контекст parent, поэтому
   итог — compact envelope ≤2000 символов. Полные результаты и evidence — в memo details.
10. **Peers.** Прямой связи между child нет: `peers: []`. Verifier берёт evidence из memo,
    файлов проекта и проверяемых источников.
11. **Memo у child.** Проверьте, видит ли субагент MCP memo. Writer без memo — путь
    независимого verifier с memo из контракта. Если memo нет ни у одного child, details
    негде хранить: `blocked: evidence_unavailable` до dispatch, не отчёт в контекст parent
    и не файловый реестр.
12. **Скилы у child.** Субагент может не видеть установленные скилы. Передайте точный путь
    к SKILL.md session-worker либо к [bootstrap](worker-bootstrap.md) с контрактом;
    при недоступных путях — inline.

Ограничения клиента вида «child не может делегировать дальше» или «child не может спрашивать
человека» совпадают с v3: только parent запускает child и говорит с пользователем.

## Ориентиры по клиентам

Это подсказка, где искать возможности, а не гарантия: клиенты меняются быстрее скила.
Всегда сверяйте фактические схемы и заполняйте `runtime.mapping`.

| Клиент | Обычно | Что проверить |
|---|---|---|
| Claude Code Desktop | `session` | [Отдельный адаптер](runtime-claude-code-desktop.md) |
| Claude Code CLI/IDE/web без инструментов сессий | `delegated` | Субагент со свежим контекстом и возвратом итога; фоновые субагенты с уведомлением = WAKE; worktree-изоляция субагента = WORKTREE `client` (создаётся от текущего HEAD — child переключается на `base_oid`) |
| DeepSeek Harness (dsh) | `delegated` | [Адаптер](runtime-deepseek-harness.md): subagent в режиме fresh context, не fork беседы; MCP memo подключён в профиле/patch-слое и виден child; модели child — только из списка моделей субагентов сессии |
| Hermes Agent | `delegated` | Делегирование задачи субагенту с возвратом итога; доступ child к MCP memo и предел параллельных субагентов |
| Другой клиент | по таблице возможностей | Нет ни `session`, ни `delegated` — `blocked: no_child_runtime` |

Имена инструментов, параметры запуска, выбор модели и пути установки скилов берите из среды
и документации клиента. Не меняйте модель, effort, permissions, MCP и настройки parent ради
запуска; выбор модели child — из реально доступных, без угадывания ID по памяти,
по профилям и порогам [выбора модели](model-selection.md).
