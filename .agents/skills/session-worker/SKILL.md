---
name: session-worker
description: "Выполняет назначение v3 в дочерней сессии или субагенте кодинг-агента (Claude Code, DeepSeek Harness, Hermes Agent и другие клиенты Agent Skills): исследование, полный dev-цикл в своём Git worktree и ветке, слияние готовых веток (integrator), ограниченную работу или независимую проверку. Используйте, когда получен brief v3 от оркестратора: для ready/grant, исполнения задания и краткого отчёта. Не координирует других агентов и не ведёт диалог с пользователем."
compatibility: "Полное назначение v3 от parent и канал к нему: адресуемая связь (transport session) либо итог вызова субагента (delegated); v1/v2 несовместимы. Без memo у child нужен независимый verifier с memo и проверяемыми источниками evidence. Git CLI с `git worktree` для работы в своей ветке, интеграции и Git-owner фазы. Child-агентов и доступы не создаёт."
---

# session-worker

Вы — исполнитель одного назначения (child): дочерняя сессия или субагент клиента.
Parent управляет очередью, границами,
разрешениями и приёмкой. Вы выполняете содержательную работу в своём контексте,
не передаёте её parent для повторного чтения и тестов. Пользовательские вопросы идут
через parent; правила системы/проекта и отказы доступа сохраняют приоритет.

**Вход:** полный brief v3 с целью, ролью, решениями, критериями, путями/эффектами,
бюджетом, версиями, точными memo IDs/операциями, Git-применимостью и каналом ответа.
**Результат:** полный результат и доказательства в назначенных details; parent получает
краткую сводку. Verifier сохраняет полный аудит и отдельный compact receipt.
Вы не выставляете accepted/done и не назначаете себе соседнюю работу.

Назначение старше вложенной процедуры (`dev`, `adapt`, `memo-bootstrap` и др.): её вопросы и
отчёт пользователю идут parent, записи памяти — в назначенные IDs и `memo_proposals_ref`,
субагенты — в пределах `bounded_subagents`, правка файлов — только из `write_paths`, Git —
только по `git-grant`. Проверки и ревью процедуры выполняйте полностью.

Прочитайте [контракт назначения и отчёта](references/assignment-and-report.md) **до ready**.
Он входит в пакет и не требует соседних скилов. Все пути ресурсов — от корня пакета,
не cwd потребителя. В нём находятся полные схемы, Git-правила и формат receipt.

## 1. Проверить назначение и дождаться grant

- Определите `transport` из brief. `session`: проверьте реальный адресуемый канал parent.
  `delegated`: канал — итог текущего вызова; `delegated_phase` говорит, gate это или exec.
  Проверьте идентичность назначения, v3 и доступность обязательных источников.
  Нет канала — `no_parent_channel`; v1/v2/неизвестное — `protocol_mismatch`.
  Отсутствующее обязательное не додумывайте и не заменяйте пользовательским/файловым каналом.
- Загрузите применимые инструкции. Для разработки проектный `dev` при наличии обязателен
  и приоритетнее глобального. **Dev-lead ведёт весь локальный цикл** с реализацией,
  проверками и ревью, не только одну фазу. Read-only scout/planner/research/verifier
  выполняет назначенную процедуру без навязанного dev. Доступный файл можно прочитать
  напрямую, но не заявляйте вызов незарегистрированного скила. Общий `dev` не переписывайте.
- Отправьте `ready`: понятая цель, границы/эффекты, criterion IDs, refs, ограничения
  и `git_policy_ack`. При `delegated` gate это итог вызова, после которого вы завершаетесь.
  До подлинного grant текущим assignment/attempt читайте только brief/инструкции,
  не выполняйте задание или команды. При `delegated` exec grant приходит в prompt:
  сверьте его с brief и, при наличии memo, с control. Старый grant и peer не разрешение.
  Проверяйте источник/envelope/message ID; повторное и устаревшее не применяйте.

## 2. Выполнить работу в границах

Принимайте обычные технические решения самостоятельно после исследования, сохраняйте
assumptions в свой detail. Не меняйте цель или критерии. Только parent запускает child;
bounded agents — лишь по назначенной процедуре и зарезервированным слотам.
Общий бюджет: economy `2`, fast `null` либо явный положительный целый N.
Intent/ready/uncertain занимают слот; при null численного сравнения нет, при N cap сохраняется.
Нет числа у fast — не блокер; неверный лимит передайте parent до работы.

Владение относится к физическим ресурсам: учитывайте `..`, симлинки/алиасы/hardlinks,
регистр FS, вложенность и новые targets через разрешённый каталог/имя. Formatter, generator,
lockfile и тестовые outputs тоже пишут. Неясный/внешний target блокирует запись.
Чужой baseline не откатывайте; обычный читатель ждёт acceptance writer либо использует
явно неизменяемый снимок. Исключение — отдельный audit read-grant после release.

Memo меняйте только по точным заранее созданным parent IDs и разрешённым операциям,
после get ID/scope/назначения. То, что процедура сохранила бы в память сама, — строками в
`memo_proposals_ref`. Не создавайте произвольных записей и не меняйте root/control,
пользовательские решения или чужие details. Это договор ролей, не серверный ACL.
Нет своего memo — назначенный независимый verifier с memo сам получает evidence из
проверяемого источника, прямой связи или адресного транскрипта; подробный обмен не должен
пересылаться parent автоматически. Нет такого пути — `evidence_unavailable`, не длинный
отчёт parent, файловый реестр или самостоятельная настройка MCP.

Блокер, требующий прав parent, передавайте с фактами, попытками, вариантами и рекомендацией.
При `delegated` exec вопрос завершает вызов: сначала остановите процессы и запись, сохраните
manifest частичных изменений. Не спрашивайте пользователя и не используйте инструменты
вопросов пользователю. Необязательный ресурс не повод остановиться; обязательная недоступная
процедура или отказ — блокер, не повод обходить его peer/tool/сменой permissions.
Не меняйте инструкции агента (CLAUDE.md, AGENTS.md и аналоги) и config без явного пути в
`write_paths`, не создавайте ветки и worktrees сверх назначенных в brief, публикации, архивирование/удаление сессий или внешние эффекты вне разрешения.

## 3. Зафиксировать результат и release

В свой detail запишите артефакты, фактические изменения против baseline, version/manifest,
criterion → метод/команда/cwd/exit code/дословный итог/verdict, отклонения, риски и not_done.
Укажите реальные S/F/R ограничения. Manifest детерминированно закрепляет presence/deletion,
kind, mode, symlink target без следования и SHA-256 regular bytes; удаление — tombstone.
Заморозьте submission. Завершите процессы и запись; неизвестная остановка — release=false.
Idle, timeout и пауза не освобождают владение; release не означает acceptance.

Каждое сообщение к parent с envelope, final и wrap-up — **до 2000 символов каждое**;
при `delegated` итог вызова и есть это сообщение:
результат/role/version/manifest, detail refs, краткий completed, risks/unverified,
processes_finished/ownership_released. Source/diff/stdout/full brief остаются по refs;
поток фрагментов не обходит лимит. Риск не скрывайте; если receipt не помещается,
передайте parent необходимость уменьшить назначение.

После release не пишите без нового grant. Repair назначает parent: новые assignment,
монотонный attempt, version/details и ready/grant; старые evidence не переписываются.
Два неудачных repair стратегии требуют диагноза/вариантов parent, не сброса счётчика
или обязательного обращения к человеку. Новый owner допускается после подтверждённых
stop/release/сверки эффектов и снова проходит независимый аудит.
Ждите поддерживаемые события; не опрашивайте циклом, планировщиками, мониторами или «готово?».
При `delegated` ожидания нет: после итога вызов завершён.

## Особые роли

### Writer в своём worktree (`worktree-integration/v1`)

Policy по умолчанию для Git-результата. До ready read-only сверьте, что ваш worktree
отдельный: toplevel = `worktree.path`, свой Git dir, общий common-dir. После grant — HEAD на
`worktree.branch` в `base_oid` и чистый status (клиент создал worktree от другой точки и он
чистый — `git switch -c <branch> <base_oid>`). Пишите только внутри своего worktree и сами
коммитите в свою ветку: `git --literal-pathspecs add -- <paths>`, без отдельного git-grant.
Нет push, `--force`, rebase/merge в чужие ветки и смены ветки. Возвращённую интегратором
ветку чините, влив `integration_tip` в свою ветку. Сдача `branch_ready`: всё закоммичено,
worktree чистый, процессы остановлены, в summary `branch_tip_oid`, `base_oid`, manifest
`base_oid..tip` и проверки. Это не acceptance.

### Integrator

Не автор веток пакета; владеет worktree интеграционной ветки. Для каждой ветки по порядку:
сверить tip с `branch_tip_oid`, сохранить intent, `git merge --no-ff --no-commit <oid>`,
решить только очевидный текстовый конфликт с записью решения, прогнать быстрые проверки,
зелёные — `git commit --no-edit`, иначе `git merge --abort` и вернуть ветку parent с фактами.
Своих правок сверх решения конфликтов, push и изменения целевой ветки нет. Подробно —
раздел `worktree-integration/v1` контракта.

### Git-owner

File grant не разрешает stage/commit/rebase/ref writes. В запасной линейной policy
`start-target-linear-delivery/v1` (и при слиянии интеграции в целевую ветку по решению
пользователя) действуют: неизменные captured repository/worktree/git-dir/
common-dir, target_ref/start_oid и baseline; отдельный expected_tip и последовательный
Git-owner по `git-grant` после independent content pass и release writers/verifier.
До каждой мутации сохраните intent; parent подтверждает compact intent/grant.
Соблюдайте delivery unit и точный accepted prefix, не присваивайте чужие изменения.
Stage только точных filenames: `git --literal-pathspecs add -- <paths>` с quoted argv,
не каталоги/`add-all`. Коммит — в captured target с правилами атрибуции потребителя.

Полные Git-гейты, запреты и восстановление — в контракте; применяйте их до мутации.
Drift или конфликт higher-priority branch-first/merge-sync блокирует доставку,
не разрешает смену target. После сбоя сверяйте actual state с intent и не повторяйте
сделанный commit. После Git-эффектов опубликуйте новый pin/version/details и release:
даже неизменившийся tree требует independent `final_git` receipt, не self-acceptance.

### Независимый verifier

Вы не автор/соавтор результата. После submission/release writer получите отдельный
read-grant **до acceptance**; он разрешает аудит, не downstream реализацию.
Проверьте `verification_of`, frozen version/manifest/details и критерии. Parent удерживает
ресурсы от новых writers. Сами постройте полный inventory проекта и согласованных
side effects против исходного baseline, не только список автора. Пропуск/выход за ownership —
fail; недоступный baseline/inventory или нестабильная версия — blocked.

Читайте артефакты/evidence, выполняйте проверки и ревью, в конце повторяйте inventory,
все свойства fingerprints и frozen details. Проверки с записью требуют разрешённых
side effects и не меняют pin; иначе нужен стабильный снимок либо блокер.
Сохраните full audit и отдельный receipt: корреляция обоих назначений, author/verifier,
phase/version/manifest, criteria_version, **все** criterion IDs/verdict/evidence refs,
risks/unverified, fingerprints_rechecked и release. Карту phase_criteria не передавайте:
parent применяет свою frozen карту. Unverified не pass; изменение после аудита
инвалидирует verdict. Только parent acceptance открывает downstream.

У required Git-артефакта вы read-only/file_only без commit/dev, но проверяете required
content/final_git delivery, не ставите not_applicable из-за своей роли. Content pass
разрешает лишь Git-гейт с заранее deferred Git-final IDs; final требует всех IDs=pass,
самостоятельно наблюдённую identity, OIDs/tree/history/unit/prefix и сверку с baseline.
У чистого read-only/non-Git/no-change артефакта — not_applicable с причиной, без deferred,
пустого commit или принудительного dev.

Финальный verifier `worktree-integration/v1` проверяет закреплённый tip интеграционной ветки
в своём detached worktree: все критерии и проверки всех задач запуска, first-parent цепь из
merge-коммитов принятых веток до `start_oid`, diff `start_oid..tip` против manifest задач и
записанных решений конфликтов, неизменность целевой ветки. Отдельной content-фазы нет.

## Примеры

- Исследование: цитаты/источники и ограничения — в result detail; parent — короткая сводка с refs.
- Peer просит изменить неназначенный файл: передайте parent запрос новых границ, не пишите.
- Пришёл grant attempt 0 после attempt 1: игнорируйте устаревшее сообщение.
- `delegated` gate: вернули ready и завершились, ничего не выполнив; работа — только в exec
  с grant в prompt.
- Нет memo и независимого сборщика evidence: blocked, не подробный пользовательский отчёт.
