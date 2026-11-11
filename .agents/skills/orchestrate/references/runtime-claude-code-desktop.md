# Адаптер v3: Claude Code Desktop (`transport: session`)

Читайте этот файл, только если текущий клиент — Claude Code Desktop с инструментами сессий.
Общая модель возможностей и выбор транспорта — в [runtime.md](runtime.md); для других клиентов
этот файл не применяется. Имена ниже относятся к Desktop, не стандарту Agent Skills.
Проверяйте фактические схемы инструментов: текст скила не создаёт возможностей.
Общие схемы, Git policy, бюджет и audit — в [contracts.md](contracts.md).

## Возможности и адреса

| Действие | Инструмент | Что подтвердить |
|---|---|---|
| Собственный ID/проект | `mcp__ccd_session_mgmt__get_session` с self | originCwd/cwd и реальный local_... ID |
| Настоящая child-сессия | `mcp__ccd_session__start_session` | Допустимый initiation, fresh prompt/background, общий checkout |
| Двусторонняя связь | `SendMessage` или `mcp__ccd_session_mgmt__send_message` | Реальный адрес и статус доставки/очереди |
| Сверка запусков | `mcp__ccd_session_mgmt__list_sessions` с linked:true | Фактические IDs/состояния своих связанных сессий |
| Сомнительный запуск | get_session / адресный list_events | Launch token, задача, источник и актуальное назначение |

Нужны все три возможности: start, адресуемая двусторонняя связь и сверка. Когда они есть,
используйте `session`: Agent/Workflow без решения пользователя не заменяют сессии, а Desktop
не подменяется CLI/tmux/SDK, новым MCP или другим скрытым механизмом. Если какой-то из трёх
нет, выбирайте транспорт по [runtime.md](runtime.md).
Сохраняйте фактические local_... IDs. Короткий name допустим как адрес только если клиент
его вернул/разрешил. Источник ready/report проверяется по каналу, не тексту sender.

## Запуск и bind

Одна цель orchestrate включает автономные child-запуски parent и следующие волны в scope,
если клиент их разрешает. Не спрашивайте на каждую волну и не меняйте initiation ради обхода.
Отказ сначала проверьте на разрешённые альтернативы; если их нет — настоящий policy blocker.

| Параметр | Значение и условие |
|---|---|
| context | `fresh`, без всей беседы parent |
| prompt | Полное назначение v3 с решениями/критериями/правами, точными доступными brief/design/dev/worker/bootstrap refs и launch token |
| background | Самодостаточный контекст минимум 80 символов, не «см. разговор» |
| initiation | `user_asked` только при действительном запросе на работу в сессиях и допустимой для tool задаче |
| use_worktree | `true` для writer и integrator в `worktree-integration/v1`; `false` для read-only ролей и запасной линейной policy |
| name/title/model | Короткие handle/title; модель из фактически доступных, в рамках решений пользователя |

До start сохраните handle/token и launch intent в memo, после — returned ID и bind.
Неясный исход/ошибка настройки не разрешают второй start: сначала установите, создан ли первый.
Parent проверяет доступность refs, child читает их напрямую. Inline нужны лишь решения человека
и нетранслируемый контекст; при недоступности bootstrap пути — сам bootstrap и его контракт.

Не передавайте cwd/host/preset/другой проект без решения пользователя и проверки назначения.
Не включайте cloud/offer там, где запрещено. Worktree для writer — по `use_worktree` клиента;
если клиент его не даёт, parent создаёт worktree сам (`git worktree add`) и передаёт путь в
brief. Нет ни того, ни другого — запасная линейная policy, не обход через другой проект.
Не меняйте parent model/effort/permissions, browser/MCP или настройки. Обычный выбор доступной
модели/режима делайте сами по цели/контексту и кратко сообщайте; явные mode/cap/budget сохраняйте.
Не угадывайте version ID модели по памяти. Профиль и модель child — по
[выбору модели](model-selection.md); ниже порога качества разрешённой альтернативы нет — blocker.

## Доставка и ожидание

Сообщения несут envelope текущего назначения. `delivered` означает доставку, `queued` —
ожидание обработки, не выполнение. Ошибка/таймаут не разрешает новую сессию. Между разными
permissions доставка может ждать подтверждения человека; неподтверждённый источник/доставка
оставляют ветку заблокированной. Автоматическое событие не является согласием пользователя.

Ждите только push-events, которые реально предоставляет клиент. Не опрашивайте
ListAgents/list_sessions в цикле, не отправляйте «готово?» и не создавайте Cron/loop/Monitor.
Ready обрабатывается адресно. Parent может завершить API-ход с сохранённым малым root/
current_frontier/control, затем по подлинному уведомлению продолжает без «продолжить?»
до финальной acceptance. Child не гарантирует пробуждение parent.

Resume читает малые записи, не весь brief/evidence/history, и не начинает разбиение заново.
Parent не читает source/diff/raw logs и не повторяет тесты. Содержательное list_events evidence
читает verifier; parent — только для конкретной сверки источника/token/управляющего блокера.
Если event-ход не допускает dispatch, выполняйте разрешённую обработку/независимую работу;
после проверки альтернатив недоступный dispatch — policy blocker, не поддельный user_asked.

## Git и клиентские ограничения

Применяйте [Git-delivery](contracts.md#git-delivery), не отдельный Desktop-вариант policy.
Captured target/start/baseline сохраняются независимо от metadata worktree клиента.
В `worktree-integration/v1` у каждого writer свой worktree и index; общие у них только refs
в common-dir, поэтому каждый двигает лишь свою ветку, а интеграционную — только integrator.
В запасной линейной policy общий checkout не даёт независимых index/refs: единственный Git-owner по отдельному git-grant
после content pass, сериализация по canonical worktree/git-dir/common-dir и общим refs.
Intent parent persist/get предшествует мутации; timeout не retry/release.

Higher-priority branch-first на default branch конфликтует с captured default target:
blocked до switch, не временная ветка вместо цели. Обязательный merge-based sync helper
(например, sync_with_base_branch) тоже означает blocked: не вызывайте merge и не обходите
обязательный helper ручной командой. Grant не отменяет реальных permission prompts;
нельзя менять permissions или поручать peer запрещённый эффект.

Полные baseline/commands/history/intent/reconcile остаются в назначенных child details.
Parent получает compact refs/OIDs/receipts. Content pass не accepted: после delivery нужен
independent final_git receipt с actual observed identity даже при unchanged tree.
Read-only verifier required артефакта сохраняет required/file_only и phase content/final_git,
не получает forced dev/commit и не подменяет результат на not_applicable.

## Навигация и жизненный цикл

Внутри Desktop ссылайтесь на реальную сессию как `[Название](#local_<реальный-id>)`:
используйте целиком возвращённый ID, не добавляйте local_ повторно. Эта ссылка не предназначена
для внешней публикации. Показ child рядом с parent возможен при поддержке клиента, но необязателен.

После release сохраняйте адрес для repair; автоматически архивировать/удалять сессии нельзя.
Worktree, созданный клиентом для сессии, убирается по правилам уборки
([параллельная доставка](parallel-delivery.md#7-уборка-и-таблица-остатков)): только влитый и чистый.
Видимость не скрывается: progress, final и permission prompts могут отображаться. Каждый
envelope/summary к parent ≤2000 символов; final и wrap-up — также каждый ≤2000. Подробная
peer-связь не должна попасть parent через auto-forward: нет безопасного канала — verifier
читает проверяемые источники. Нет доступа/evidence — blocked evidence_unavailable, не verbose
или file fallback. Не просите peer выполнить то, что запрещено вашей сессии.
