# Memo v3: малый control и отдельные details

Читайте при создании записей, смене control и resume. Нужен настроенный пользователем
AgentMemory MCP: memory_recall/get/save/update, для снятия зависимости — unlinks.
Проверяйте реальные схемы. Нет нового KV API, ACL, транзакций, CAS/lease, compact-read
или нативного статуса blocked. Memory_get читает content целиком: разные бюджеты чтения
требуют физически разных записей. Общий протокол — [contracts.md](contracts.md).

## Scope до записи

1. Найдите ожидаемый scope в проектном протоколе/доступной конфигурации, не по имени папки.
   Не читайте .env/секреты и не настраивайте MCP.
2. Подтвердите scope адресным get известной проектной записи. Recall лишь находит кандидата;
   пустой/похожий результат ничего не доказывает. Нет известной записи — нужно явное
   подтверждение scope/подключения пользователем до save, не пробная запись.
3. Save всегда с явным scope:project:...; get/update не получают выдуманный scope-параметр.
   Перед каждым update подтвердите ID через get и сверьте scope/назначение.

Без рабочего memo parent/подтверждённого scope нет dispatch и файлового fallback. Сбой записи
останавливает новые start/grant, но не освобождает работающих владельцев; несохранённый
переход не считается записанным. Child без memo допустим только при независимом verifier
с memo и проверяемым доступом к evidence, не через полный report parent.

## Разделение записей и single writer

| Запись | Содержимое | Writer → обычный читатель |
|---|---|---|
| Root (task/epic) | Цель, parent, mode, current_frontier, refs решений/истории | Parent → parent |
| Task/control фронта | Assignment, role/session, launch, grants/release, владение, pins/IDs | Parent → parent/назначенный child |
| Brief/design detail | Полная постановка, решения/критерии, доступные source refs | Один назначенный planner/dev-lead либо parent → исполнитель |
| Result/evidence detail | Полный результат, manifest, проверки/ограничения | Автор → verifier/принятый следующий потребитель |
| Verification detail | Полный независимый audit | Verifier → непосредственный потребитель/адресный спор |
| Предложения в память (`memo_proposals`) | Строки будущих записей: kind, теги, summary, текст | Автор → verifier → parent сохраняет после acceptance |
| Compact receipt | Краткая схема receipt с phase/Git refs/OIDs, без evidence | Verifier → parent |
| Git baseline/intent/delivery detail | Полные initial status/index/tree, commands/OIDs/inventory/reconcile | Один scout/Git-owner на ID → verifier |
| History index | Refs закрытых control/версий, не evidence | Parent → адресное восстановление |

Parent заранее создаёт малые placeholders через save, проверяет scope get, назначает точный
ID, единственного writer, версию и update-операции (обычно content/summary). До bind writer
обозначен назначением, после start связан с фактическим actor в control до grant. Создатель
placeholder не второй автор результата; общей записи «всему scope» нет. Это правила ролей,
не ACL и не замена клиентским permissions.

Child обновляет только свои точные IDs/версии. Root/control, решения человека, grants,
ownership/acceptance — parent-only. Содержательные требования dev выполняются в назначенном
detail; предложение изменить control/создать другую запись идёт parent с ref, не в общий dev.
Details не переписываются на status. Submission закрепляет content/артефакты по протоколу;
repair получает новые assignment/artifact_version/detail/receipt IDs. Старые доступны verifier,
не исправляются задним числом; writer/версия прежнего ID молча не меняются.

## Приём задач из memo

Берите только задачи, названные пользователем (ID, эпик/root и его `part_of` подзадачи), каждую
адресным get. Recall не расширяет набор. `blocked_by` внутри набора — рёбра `hard` карты;
`blocked_by` на задачу вне набора — узел `blocked` в `work_map.blocked_outside`, без
подтягивания блокера. Подзадачи декомпозиции большой задачи parent сохраняет как task
(type=task, `part_of` к большой задаче, `blocked_by` по рёбрам `hard`), предложенные planner
в его detail. Status каждой задачи ведите как обычно: `doing` с branch задачи при старте,
`done` — после acceptance всего запуска по проектному циклу.

## Нативные типы, статусы и зависимости

Root: kind=task, type=epic, summary обязателен, status=todo. Control: kind=task, type=task,
part_of к root. Details: episodic/semantic по предмету с явным назначением. Артефакты задачи
могут быть файлами, координация task/status/handoff/log — нет. Не копируйте heartbeat/транскрипты.

Статусы: backlog/todo/doing/review/testing/release/done. При doing укажите фактическую branch
(пустую для не-Git) и workflow=orchestrate. Review — реальный PR, release — влито и ждёт релиза;
accepted не значит released. Исследование done после принятой доставки, разработка — по
проектному циклу. Acceptance отдельно в control.

Blocked_by идёт от заблокированной task к task-блокеру. До добавления верните task в todo,
сохранив ход компактно в control. Реальный блокер разрешён — закройте его или снимите точную
связь unlinks с причиной; не объявляйте код выпущенным ради снятия зависимости.
Verifier ждёт submission/release и audit pin, не acceptance writer. Downstream ждёт accepted.

## Малый root и control

Поля ниже — content, не аргументы MCP. Root не хранит full brief или IDs всех закрытых задач.

```yaml
protocol: v3
run_id: <устойчивый ID, например root ID>
project: <проверенный корень>
scope: <project:...>
parent_session_id: <фактический parent либо null, если клиент ID не даёт>
runtime: <client, transport session|delegated, capabilities, mapping, limits — см. [runtime](runtime.md)>
mode: fast # economy только по просьбе пользователя
active_limit: null # fast: null (безлимит) либо явный положительный целый N; economy: 2
git_policy: worktree-integration/v1 # start-target-linear-delivery/v1 | not_applicable
integration_ref: <refs/heads/orch/<run>/integration либо null>
work_map_ref: <detail полной карты зависимостей либо null>
work_map: <компактная карта: узлы со статусами writing|branch_ready|integrated|returned|blocked, рёбра hard/soft, critical_path; см. parallel-delivery.md>
worktrees: [] # {task, path, branch, provided_by, state} — для уборки и таблицы остатков
phase: planning # running | paused | accepted
revision: 0 # номер изменения, не lock
current_frontier: [] # только актуальные control IDs и ближайшие зависимости
history_refs: [] # компактные ссылки на отдельный индекс, не растущий журнал
current_decision_refs: []
blocker_refs: []
```

Активный бюджет трактуется по [контракту](contracts.md#бюджет-и-физическое-владение):
economy=2, fast=null либо явный положительный целый N; null снимает только числовой cap.
При завершении ветки перенесите ref из current_frontier в отдельный history index.
При росте history/decisions добавляйте отдельные индексы, не архив внутри root; историю
не удаляйте и не сканируйте на обычном status.

Control содержит только текущие управляющие данные:

- protocol/run/task, role, assignment_id/attempt/repair_count, transport, session_id
  (при delegated — ID вызова либо handle `<assignment_id>/<gate|exec>`);
- launch_name/token/state (reserved|intent|bound|uncertain), текущие зависимости;
- model: profile/model_id/effort/reason/selection по [выбору модели](model-selection.md);
- компактные физические read/write/side-effect границы, owner, grant/release;
- acceptance (pending|rejected|accepted|paused), audit reservation;
- brief/design/result/evidence/verification/receipt IDs, writer/операции, version/manifest pin;
- текущие обработанные message IDs; до grant — полный frozen expected_criterion_ids,
  criteria_version и phase_criteria, утверждённые parent;
- обязательный git_delivery, его captured identities/OIDs и refs полного baseline/intent.

Нет full brief/stdout/журнала сообщений. Слишком большую границу разбейте до dispatch,
не теряя скрытых конфликтов ради компактности. Child читает полный brief/detail напрямую.
Критерии/phase map после resume берутся из control, не из receipt или полного brief;
новые определения требуют новой версии/назначения.

## Git control и сохранение переходов

Схема и обязательные гейты — [Git-delivery](contracts.md#git-delivery). Capture готовит
назначенный scout/lead; parent до изменений сохраняет compact canonical identities,
start target/start_oid(unborn), expected_tip и refs immutable baseline/initial_index/
initial_status, approved preexisting, policy/allowed paths. Applicability=required либо
явное not_applicable с причиной. Read-only verifier required артефакта не меняет applicability.
Start target/start/baseline неизменны при commit/resume; no-change сохраняет имеющийся capture.

Каждый Git-grant сохраняет delivery_unit_id, pre_git_oid и accepted_prefix_ref/fingerprint:
только уже final accepted prefix, без pending sibling. Полный ledger читает verifier;
новая unit не присваивает sibling paths. Исходный baseline не заменяется prefix tip.
Ready/grant подтверждают git_policy_ack, не implicit v2 defaults. File grant не Git grant.

State: captured → content_pending → content_pass → git_granted → intent → delivered →
final_pending → accepted; crash → uncertain, нарушение → blocked/paused acceptance.
Content pass/release/delivered/снятие reservation сами по себе не accepted.
Git-owner получает отдельное назначение/ready/git-grant после writer и content verifier
release. Parent persist/get подтверждает owner/grant/expected_tip и compact intent ref до
каждой Git-мутации; full intent/checkpoint — в назначенных owner details. После эффекта
expected_tip/state/refs обновляются только по сверенной корреляции. Это не CAS/exactly-once.
Final receipt обязателен даже при unchanged tree; drift требует новой версии/проверки.

## Dispatch, приёмка и repair

Сохраняйте переход **до** соответствующего эффекта:

| Момент | Что сохранить/проверить |
|---|---|
| До dispatch | Control в todo, scoped placeholders, успешный индекс в current_frontier; зависимости, слоты всех ролей/bounded agents и physical write/write/read/write/side effects |
| До start | Assignment/attempt, version, token/handle, launch intent; start один раз |
| До grant | Bind returned real ID и writers; подлинный ready; persisted grant/doing с branch/workflow и ownership |
| По report | Источник/IDs/дедупликация, frozen submission pin и фактический release; старый report не освобождает текущего owner |
| До audit | Reservation от новых writer; независимый verifier, своё назначение/ready/read-grant после release, до acceptance writer |
| По receipt | Источник/корреляция/независимость, version/manifest, criteria_version, точные expected IDs без пропусков/дубликатов/лишних, статусы по собственной phase map, финальная сверка и release |
| До downstream | Явная acceptance=accepted: independent final_git pass либо not_applicable pass; content даёт только content_pass |
| До repair | Release и отдельное решение о reservation; новые assignment/version/IDs/ready/grant; downstream остаётся закрыт |

Audit выполняет verifier по контракту: полный actual inventory вне списка автора, manifest/
ownership/criteria, проверки и повторная сверка pin/content в конце; full evidence отдельно
от compact receipt. Parent его не перечитывает и не повторяет тесты.
Критические intent/bind/grant/ownership/pin/acceptance подтвердите get малого control до
следующего эффекта. Ошибка save/index может оставить orphan, intent/bind — сессию без ID:
нужна сверка, не повтор start. Неподтверждённая доставка не считается выполнением.

Обычный repair у того же actor, два на стратегию. После двух неудач parent автономно
диагностирует и меняет подход, не требует human pause по счётчику. В context/decisions/
history refs сохраняются стратегия, repair_count, диагноз/progress и причина смены;
attempt монотонен. Новые IDs/версия/ready/grant и independent audit обязательны.
Смена actor — только после реальной остановки/release и остаточных эффектов.
Нет новых данных/безопасной альтернативы — доказанный unresolved blocker, не endless fanout.

Ожидание push-event сохраняет малый control, refs стратегии/попыток/ожидаемое событие;
API-ход может закончиться, задача — нет. По подлинному уведомлению продолжайте до final
acceptance без «продолжить?», без опроса циклом, планировщиками или мониторами. Обычный выбор делается автономно;
человеку через parent нужен лишь физический шаг или доказанный unresolved/policy blocker.
До вопроса выполните независимое, сохраните факты/попытки/состояние/unverified в details и refs
в control; один пакет с рекомендацией/последствиями/Other просит минимальное действие.
Цель/acceptance/явные лимиты сохраняются, отказ не обходится ролью/tool/permissions.

## Resume без архива

1. По root ID подтвердите точную запись get; recall лишь кандидаты. Неоднозначность сначала
   разберите по доступным данным; если безопасного решения нет — блокер через parent.
   Не создавайте параллельный root на те же ресурсы до сверки прежнего.
2. Проверьте прежнего parent в реальном клиенте (сохранённый `runtime` в control). Если он
   ещё управляет — нужна согласованная передача; idle/revision не доказывают остановку/владение.
   Незавершённые назначения сверяйте по их сохранённому transport/mapping; новый transport
   применяется только к новым назначениям.
3. Читайте только root/current_frontier control и нужные текущие compact receipt/решения,
   не весь history/brief/design/result/evidence. Детали загружает назначенный recovery child.
   Mode/active_limit сохраняются точно: null/N/economy 2, без clamp/default. Невалидное или
   отсутствующее поле сохранённого root требует уточнения; это не ответ «fast» без числа.
4. Intent/uncertain адресно сопоставьте с session/token. Единственное точное совпадение —
   bind после проверки; неоднозначность/недоступный реестр — paused. Отсутствие в одном
   списке не доказывает несостоявшийся start; бесконечного polling нет.
5. Потерянный writer/Git-owner остаётся владельцем до проверки остановки, фоновых процессов,
   операции и остаточных эффектов. Stop/delete запрос и timeout сами не доказательство.
   Crash commit сверяется с intent, не дублируется; unknown outcome/чужая операция/смена branch —
   paused без нового target. Не --skip/foreign abort. Новый parent/assignment сообщается
   подлинным каналом; неподтверждённая передача остаётся paused.
