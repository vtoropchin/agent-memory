# Jev: дешёвые суждения для выбора модели

Читайте на шаге 1 (режим) и перед калибровкой назначения или эскалацией после repair,
если `runtime.jev.mode` не `off`. Jev — модель-«решатель» класса System One (TypeSafe AI;
совместимый провайдер — Jev AI): на типизированный вопрос она возвращает вероятности
и не пишет текст или код. **Jev даёт суждения, решение принимает таблица в
`scripts/jev.py`** — так правило «дешёвая модель не принимает решений» сохраняется.

Зависимость необязательная. Нет ключа, `python3` (≥ 3.9), shell или сети до хоста
провайдера — `mode: off`, выбор модели идёт как без Jev.

## Конфигурация

Скрипт сам читает переменные окружения процесса, иначе `.env` в корне основного checkout
проекта (через `git rev-parse --git-common-dir`; в worktree child `.env` нет):

| Переменная | По умолчанию | Смысл |
|---|---|---|
| `JEV_AI_API_KEY` | — | Ключ Jev AI; без него — `TYPESAFE_API_KEY` |
| `JEV_AI_BASE_URL` | `https://jev-ai.pro/api` | Корень API (`/v1/systemone`, `/v1/models`) |
| `JEV_AI_MODEL` | `jev-latest` | Модель |
| `TYPESAFE_API_KEY`, `TYPESAFE_BASE_URL`, `TYPESAFE_DEFAULT_MODEL` | `https://api.typesafe.ai` | Официальный провайдер, если ключа Jev AI нет |

Адрес и модель берутся из того же источника, что и ключ: ключ из окружения не уйдёт на
адрес из `.env` чужого репозитория. Редиректы не выполняются; к `http://localhost` запросы
идут мимо прокси.

Запускайте скрипт из каталога проекта (любой его подкаталог или worktree) либо с
`--project <корень проекта>` перед командой: иначе он ищет `.env` не там.

Записать их можно через `make install` набора (спрашивает адрес, модель и ключ) или вручную.
**Вы `.env` не открываете и ключ не просите в чате:** ключ читает только скрипт; он не
попадает в вывод, memo, brief и логи. Child-ам ключ не нужен — Jev вызывает только parent.
Локальная совместимая модель подключается тем же `JEV_AI_BASE_URL` (http:// — только
для localhost). Пример — открытый Kev (`make install` набора предлагает его вместо облачного
Jev и печатает инструкцию запуска). Свой адрес и своё имя модели (например, `kev-latest` на `127.0.0.1`) —
штатная настройка, а не расхождение со скилом: назовите хост и модель в отчёте, но не
вносите их в список расхождений.

## Режимы (`runtime.jev` в control)

```yaml
jev:
  mode: off | shadow | apply
  provider: jev-ai | typesafe | null   # из `jev.py config`
  host: <хост из jev.py check>
  consent_ref: <id записи memo с согласием на отправку данных | null>
```

1. На шаге 1 выполните `python3 <пакет>/scripts/jev.py config` — всегда, даже если уверены,
   что ключа нет: режим определяет команда, а не догадка. Код 3 — `mode: off`, одной
   строкой сообщите, как включить (`make install` или `.env`), и продолжайте.
2. Иначе `jev.py check` (без инференса, баланс не тратит): назовите пользователю хост и
   провайдера — это фактический адрес, куда уйдут данные. Ошибка — `mode: off` с причиной.
3. **Согласие.** В Jev уходят цель, критерии и пути назначений (без кода, диффов и секретов),
   для repair — отчёт о неудаче. Перед первым вызовом в проекте одной строкой скажите, что и
   на какой хост уйдёт, и получите согласие; запишите его в memo (`consent_ref`). Есть
   запись — не спрашивайте снова. Нет согласия — `off`.
4. По умолчанию `shadow`. `apply` — только по явному решению пользователя, лучше после отчёта
   о совпадениях в `shadow`.

| Режим | Что применяется |
|---|---|
| `off` | Ваша калибровка, как в [выборе модели](model-selection.md) |
| `shadow` | Ваша калибровка; ответ Jev записывается рядом для сравнения |
| `apply` | Профиль и уровень усилия из `calibrate`; вы можете только **поднять** их, указав причину. Выбирать нечего — `calibrate` не вызывается, всё по таблицам |

## Калибровка назначения

Только когда есть что выбирать: `selection: chosen` или `effort_selection: chosen`.
Нельзя выбрать ни модель, ни уровень усилия — вызов лишь тратит баланс. Одна модель в
каталоге child (`inherit`, `single_candidate`) — не повод пропустить вызов, если уровень
усилия выбирается: калибровка даёт и профиль для проверки порога, и `effort`.

Вызов или пропуск — часть гейта запуска (`SKILL.md`, шаг 3): до start в control назначения
есть блок `jev` с ответом либо `skipped: mode_off | scout | nothing_to_choose`. Других
причин пропуска нет; «маршрут один», «торопился», «решил, что не нужно» — не причины.

Перед intent назначения подготовьте state и вызовите
`python3 <пакет>/scripts/jev.py calibrate --state <файл|->` (state можно подать в stdin):

```json
{
  "role": "worker",
  "goal": "Добавить поле email в форму профиля",
  "acceptance_criteria": ["поле сохраняется", "валидация формата"],
  "planned_paths": ["src/profile/form.py", "tests/profile/test_form.py"],
  "writes_code": true,
  "read_only": false,
  "review_md_risk": "low",
  "author_profile": null,
  "top_policy": "table"
}
```

- `role`: `scout` | `planner` | `dev-lead` | `worker` | `integrator` | `verifier`.
- `top_policy`: `table` (по умолчанию) или `escalation_only` из правил проекта
  ([выбор модели](model-selection.md), «Правила проекта»). При `escalation_only` ответ даёт
  `profile: coding` вместо `top` с правилом `top_by_escalation_only`; `table_profile` —
  профиль по таблице до этого ограничения: сохраните его для эскалации.
- `read_only`: назначение ничего не пишет — ни файлов, ни данных, ни внешних эффектов.
  Не указано — считается записью (для scout — `true`; scout с записью — ошибка входа).
- `writes_code`, `read_only`, `review_md_risk` (`low`/`medium`/`high`/`unknown`) и `author_profile`
  (только для verifier) — **факты**, которые вы определяете сами по brief и `REVIEW.md`;
  в Jev они не отправляются, из них таблица строит пол.
- В Jev уходят только `goal`, `acceptance_criteria`, `planned_paths`. Не кладите туда
  код, секреты и персональные данные.

Ответ (код 0):

```json
{"profile": "coding", "table_profile": "coding", "effort": "medium", "rules": ["writes_code_floor", "small_and_clear"],
 "judgments": {"size": {"score": 0.6, "p_large": 0.05},
               "secrets": {"p": 0.04, "verdict": "no"}, "...": "..."},
 "model": "jev-1.14.0", "usage": {"input_tokens": 120, "output_tokens": 0}}
```

Для `scout` Jev не вызывается и ключ не нужен; ответ —
`{"profile": "light", "table_profile": "light", "effort": "low", "rules": ["scout_facts_only"], "judgments": null, "model": null, "usage": null}`.

Таблица (в скрипте; пороги вероятности «да» ≥ 0.7, «нет» ≤ 0.3, между — «неясно»):

| Условие | Профиль не ниже |
|---|---|
| `role` = `scout` | всегда `light`; Jev не вызывается (`scout_facts_only`); scout с `writes_code` — ошибка входа |
| `writes_code` | `coding` |
| `review_md_risk` = `high` или `unknown` — если не `read_only` или роль решающая | `top` |
| Затронуты права, секреты, внешний ввод, хранимые данные или публичный контракт — «да» **или «неясно»**, с тем же условием | `top` |
| Worker с `read_only`: риск не применяется (`read_only_risk_not_applied`) | — |
| P(T2 или T3) ≥ 0.3, либо требования неоднозначны / нужен выбор архитектуры (не «нет») | `top` для planner, dev-lead, integrator, verifier; `coding` для worker |
| Иначе: planner, dev-lead, integrator, verifier | `coding` |
| Иначе: worker | `light` |
| verifier | не ниже `author_profile` |

Решающие роли — planner, dev-lead, integrator, verifier. Итог — максимум сработавших строк. Пороги качества из [выбора модели](model-selection.md)
(код не ниже `coding`, verifier ≥ автора, понижения нет) действуют поверх: результат,
который их нарушает, не применяйте. Профиль затем превращается в конкретную модель по
каталогу (`model-selection.md`, «Каталог моделей»), а `effort` — в значение клиента по
разделу «Уровень усилия» там же (`light` → `low`; `coding` → `medium`, при крупной задаче,
риске, verifier/integrator, а при неясных требованиях — только у решающих ролей — `high`;
`top` → `high`, T3 с риском — `max`).

Запишите в control назначения рядом с `model`:

```yaml
jev:
  mode: shadow
  profile: coding          # ответ скрипта
  rules: [writes_code_floor, small_and_clear]
  applied: false           # true только в apply и если профиль взят из ответа
  skipped: null            # или mode_off | scout | nothing_to_choose — тогда profile/rules: null
```

## Причина неудачного repair

Перед эскалацией профиля после неудачного repair:
`python3 <пакет>/scripts/jev.py repair-cause --state <файл|->` с

```json
{"failure_report": "<краткий отчёт child о неудаче>", "check_output": "<хвост вывода проверки>",
 "failed_repairs": 1, "current_profile": "coding",
 "current_effort": "medium", "effort_value_count": 3,
 "top_policy": "table", "high_risk": false}
```

| `action` | Что делать |
|---|---|
| `raise_effort` | Ошибка рассуждения (p ≥ 0.7) после первого repair на `coding`/`top`, и у клиента есть значение уровня выше текущего: новая попытка на той же модели с `target_effort` (первый уровень, дающий другое значение клиента). `effort_value_count` — число значений в `effort_values` модели; без него и `current_effort` не выдаётся |
| `escalate` | Причина — ошибка рассуждения (p ≥ 0.7) после двух repair: новая стратегия на `target_profile` |
| `blocker` | Среда, доступ или нехватка данных (p ≥ 0.7): эскалация модели не поможет, это блокер |
| `parent_decides` с `top_reserved_for_high_risk` | `top_policy: escalation_only`, два неудачных repair на `coding`, но задача не высокого риска (`high_risk: false` — `table_profile` калибровки не был `top`): `top` не назначается, решение parent |
| `parent_decides` | Таблица не решает: правила «Эскалация вместо дорогого старта» как без Jev |

В `shadow` ответ только записывается; в `apply` `blocker`, `raise_effort` и `escalate` применяются.

## Ошибки

| Код | `error` | Что делать |
|---|---|---|
| 2 | `bad_input`, `state_too_large` | Исправьте state (сократите текст) и повторите один раз |
| 3 | `not_configured`, `bad_config` | `mode: off` до конца запуска; одна строка пользователю |
| 4 | `unavailable`, `bad_response` | Скрипт уже повторил запрос. Это назначение калибруйте сами; после трёх подряд — `off` |
| 5 | `api_error` (`status` 401/402/403/429) | Ключ, баланс или лимит: `off`, сообщите пользователю статус |

Ошибка — не ответ: не трактуйте её как «нет» и не понижайте профиль из-за неё.

## Отчёт

В итоге работы: режим, хост, число вызовов и пропусков по кодам (их сумма равна числу
запусков child), токены из `usage`, а в `shadow` —
сколько раз профиль Jev совпал с вашим, был ниже и выше (с ролями). Экономию лимитов
называйте только по замерам клиента.
