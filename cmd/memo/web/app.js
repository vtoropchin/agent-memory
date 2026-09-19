// app.js — вся логика web-морды AgentMemory (задача 6.3). Ванильный JS
// (fetch + DOM), без фреймворков. Потребляет JSON API из cmd/memo/web.go
// "как есть" (см. DESIGN.md §12, Этап 6). Весь пользовательский текст
// (content/tags/scope/summary/source) вставляется в DOM только через
// textContent — никакого сырого innerHTML с данными из API.
//
// С Этапа 15 (задача 15.6) в том же файле живёт экран kanban-доски (#/board):
// hash-роутер, панель фильтров, колонки/свимлейны, нативный HTML5 drag&drop и
// расширение модалки карточки для задач. Логики доски здесь нет — колонки,
// прогресс и блокировки считает internal/memory/board, фронт только рисует
// ответ GET /api/board и шлёт PATCH /api/memories/{id} (DESIGN.md §12).

"use strict";

// --- цвета бейджей kind (литеральные классы нужны здесь для Tailwind-сканера
// @source, который ищет полные имена классов текстом в файле) ---
const KIND_BADGE_CLASS = {
  semantic: "bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200",
  episodic: "bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200",
  procedural: "bg-purple-100 text-purple-800 dark:bg-purple-900 dark:text-purple-200",
  debt: "bg-orange-100 text-orange-800 dark:bg-orange-900 dark:text-orange-200",
  task: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200",
};
const KIND_BADGE_CLASS_DEFAULT = "bg-slate-100 text-slate-800 dark:bg-slate-700 dark:text-slate-200";

const KIND_LABEL = { semantic: "семантика", episodic: "эпизод", procedural: "процедура", debt: "тех-долг", task: "задача" };

// --- бейдж стадии задачи (kind=task, задача 14.1, стадии доски — Этап 15):
// memory.Status -> подпись и цвет. Литеральные классы — для Tailwind-сканера
// @source, как и у KIND_BADGE_CLASS. ---
const STATUS_LABEL = {
  backlog: "бэклог",
  todo: "к работе",
  doing: "в работе",
  review: "ревью",
  testing: "тестирование",
  release: "к релизу",
  done: "готово",
};
const STATUS_BADGE_CLASS = {
  backlog: "border-dashed border-slate-300 text-slate-500 dark:border-slate-600 dark:text-slate-400",
  todo: "border-slate-300 text-slate-600 dark:border-slate-600 dark:text-slate-400",
  doing: "border-blue-300 text-blue-700 dark:border-blue-700 dark:text-blue-400",
  review: "border-violet-300 text-violet-700 dark:border-violet-700 dark:text-violet-400",
  testing: "border-amber-300 text-amber-700 dark:border-amber-700 dark:text-amber-400",
  release: "border-teal-300 text-teal-700 dark:border-teal-700 dark:text-teal-400",
  done: "border-emerald-300 text-emerald-700 dark:border-emerald-700 dark:text-emerald-400",
};
const STATUS_BADGE_CLASS_DEFAULT = "border-slate-300 text-slate-600 dark:border-slate-600 dark:text-slate-400";
// STATUSES — порядок стадий доски (= memory.Statuses) для селекта стадии в
// модалке; колонки доски идут в порядке ответа сервера, не отсюда.
const STATUSES = ["backlog", "todo", "doing", "review", "testing", "release", "done"];

// statusBadge — чип стадии задачи или null, если у записи статуса нет (все
// kind, кроме task). Вызывающий код добавляет результат только при непустом
// значении — чтобы карточки прочих типов памяти выглядели ровно как раньше.
function statusBadge(status) {
  if (!status) return null;
  return el("span", {
    class: "rounded-full border px-2 py-0.5 text-xs font-medium " + (STATUS_BADGE_CLASS[status] || STATUS_BADGE_CLASS_DEFAULT),
    text: STATUS_LABEL[status] || status,
  });
}

// --- бейдж типа задачи (memory.TaskType, Этап 15): показывается только для
// epic/story — обычная задача (task, значение по умолчанию) бейджа не получает,
// как и в строке брифа recall. Read-only; правка типа — доска (15.5). ---
const TASK_TYPE_LABEL = { epic: "эпик", story: "история" };
// TYPE_LABEL — все типы задачи (= memory.TaskTypes) для селектов/фильтра доски;
// TASK_TYPE_LABEL выше — только те, что получают бейдж.
const TYPE_LABEL = { epic: "эпик", story: "история", task: "задача" };
const TYPES = ["epic", "story", "task"];
const TASK_TYPE_BADGE_CLASS = {
  epic: "bg-purple-100 text-purple-800 dark:bg-purple-900 dark:text-purple-200",
  story: "bg-sky-100 text-sky-800 dark:bg-sky-900 dark:text-sky-200",
};

// taskTypeBadge — чип типа задачи или null (не задача, либо тип task).
function taskTypeBadge(type) {
  if (!TASK_TYPE_LABEL[type]) return null;
  return el("span", {
    class: "rounded-full px-2 py-0.5 text-xs font-medium " + TASK_TYPE_BADGE_CLASS[type],
    text: TASK_TYPE_LABEL[type],
  });
}

// --- бейдж источника кандидата поиска (задача 10.1): "lexical"/"vector"/
// "lexical+vector" (Candidate.Source, см. DESIGN.md §8/§12, задача 8.1) -> короткая
// подпись на бейдже. Литеральные классы нужны здесь для Tailwind-сканера @source. ---
const SOURCE_LABEL = { lexical: "fts", vector: "vec", "lexical+vector": "fts+vec" };
const SOURCE_BADGE_CLASS = {
  lexical: "border-slate-300 text-slate-600 dark:border-slate-600 dark:text-slate-400",
  vector: "border-emerald-300 text-emerald-700 dark:border-emerald-700 dark:text-emerald-400",
  "lexical+vector": "border-blue-300 text-blue-700 dark:border-blue-700 dark:text-blue-400",
};
const SOURCE_BADGE_CLASS_DEFAULT = "border-slate-300 text-slate-600 dark:border-slate-600 dark:text-slate-400";

// --- стили кнопок (стандартный компонент Tailwind: solid primary danger +
// secondary). Литеральные строки классов нужны Tailwind-сканеру @source ---
const BTN_DANGER =
  "inline-flex items-center gap-x-1 rounded bg-red-600 px-2 py-1 text-xs font-semibold text-white shadow-sm hover:bg-red-500 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-red-600 disabled:opacity-50 dark:bg-red-700 dark:hover:bg-red-600";
const BTN_SECONDARY =
  "inline-flex items-center rounded bg-white px-2 py-1 text-xs font-semibold text-slate-700 shadow-sm ring-1 ring-inset ring-slate-300 hover:bg-slate-50 disabled:opacity-50 dark:bg-slate-700 dark:text-slate-100 dark:ring-slate-600 dark:hover:bg-slate-600";

// --- маленький DOM-хелпер: строит элементы без innerHTML, чтобы весь
// пользовательский текст шёл через textContent (жёсткое требование задачи) ---
function el(tag, opts, children) {
  opts = opts || {};
  const e = document.createElement(tag);
  if (opts.class) e.className = opts.class;
  if (opts.text !== undefined) e.textContent = opts.text;
  if (opts.attrs) {
    for (const k in opts.attrs) e.setAttribute(k, opts.attrs[k]);
  }
  if (opts.onClick) e.addEventListener("click", opts.onClick);
  (children || []).forEach(function (c) {
    if (c) e.appendChild(c);
  });
  return e;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

// --- SVG-иконка: статичная разметка (не пользовательские данные) строится через
// createElementNS, а не innerHTML, чтобы не нарушать правило «текст только через
// textContent». cls — Tailwind-классы иконки (напр. "-ml-0.5 size-5") ---
const SVG_NS = "http://www.w3.org/2000/svg";
function icon(pathD, cls) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("class", cls);
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.5");
  svg.setAttribute("aria-hidden", "true");
  const p = document.createElementNS(SVG_NS, "path");
  p.setAttribute("stroke-linecap", "round");
  p.setAttribute("stroke-linejoin", "round");
  p.setAttribute("d", pathD);
  svg.appendChild(p);
  return svg;
}
// Heroicons «trash» (outline).
const TRASH_PATH =
  "m14.74 9-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 0 1-2.244 2.077H8.084a2.25 2.25 0 0 1-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 0 0-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 0 1 3.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 0 0-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 0 0-7.5 0";

// --- API ---

async function api(path, opts) {
  const resp = await fetch(path, opts);
  let body = null;
  try {
    body = await resp.json();
  } catch (e) {
    // не JSON — оставляем body = null, сообщение соберём из статуса
  }
  if (!resp.ok) {
    const msg = (body && body.error) ? body.error : ("HTTP " + resp.status);
    const err = new Error(msg);
    // status/body — для ветвления по коду и тела 409 {error, blockers} (15.6):
    // тост переноса заблокированной карточки перечисляет блокеров по именам.
    err.status = resp.status;
    err.body = body;
    throw err;
  }
  return body;
}

// apiDelete — мягкое удаление записи (DELETE /api/memories/{id}): запись
// становится архивной (тумстоун), но не стирается безвозвратно.
function apiDelete(path) {
  return api(path, { method: "DELETE" });
}

// apiPatch — правка метаданных записи (PATCH /api/memories/{id}, задача 10.2):
// importance/tags — сигналы ранжирования и группировки, а с Этапа 15 (15.6) —
// рабочие метаданные задачи: status/type/branch/workflow и связи links/unlinks
// (перенос по доске, родитель, блокеры). content/kind/scope/summary морда не
// отправляет никогда — содержание фактов меняет только агент через MCP.
function apiPatch(path, body) {
  return api(path, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// --- глобальное состояние фильтров/пагинации ---

const state = {
  query: "",
  // mode — режим /api/search (задача 10.1): "semantic" (дефолт, гибрид FTS5+вектор)
  // или "lexical" (прежнее поведение, чистый FTS5). Не влияет на просмотр
  // (/api/memories не принимает mode).
  mode: "semantic",
  scopes: new Set(),
  kinds: new Set(),
  showSuperseded: false,
  limit: 20,
  offset: 0,
  total: 0,
  exhausted: false, // все страницы просмотра уже подгружены
  // controller: единственный признак «загрузка идёт» (не null ⇒ есть in-flight
  // запрос) и одновременно ручка отмены для вытеснения при смене фильтров.
  controller: null,
  // view — текущий экран по hash-роутеру (задача 15.6): "list" (#/) или "board" (#/board).
  view: "list",
  // listLoaded — список «Память» уже загружался (грузится лениво при первом показе).
  listLoaded: false,
  // board — состояние экрана доски. Источник истины — URL-хэш (#/board?scope=…):
  // смена фильтра пишет хэш, hashchange разбирает его обратно в это поле и
  // грузит доску — так ссылка воспроизводит экран, а загрузка идёт одним путём.
  board: {
    scope: "",          // обязательный scope проекта (селект из stats.byScope)
    types: new Set(),   // фильтр type (повторяемый параметр)
    parent: "",         // «только дети» — id эпика/истории
    parentRef: null,    // подпись выбранного родителя для пикера ({id, summary, …})
    branch: "",
    workflow: "",
    tag: "",
    doneLimit: 0,       // 0 — серверный дефолт (30); «показать ещё» удваивает
    groupByEpic: false, // свимлейны по родителю
    data: null,         // последний ответ GET /api/board
    controller: null,   // in-flight запрос доски (вытеснение при смене фильтров)
  },
};

let statsCache = null;

// --- форматирование ---

function fmtDate(unixSeconds) {
  if (unixSeconds === null || unixSeconds === undefined) return "—";
  const d = new Date(unixSeconds * 1000);
  return d.toLocaleString("ru-RU");
}

function fmtStars(importance) {
  const n = Math.max(0, Math.min(5, importance || 0));
  return "★".repeat(n) + "☆".repeat(5 - n);
}

function firstLines(text, maxLen) {
  if (!text) return "";
  const trimmed = text.trim();
  if (trimmed.length <= maxLen) return trimmed;
  return trimmed.slice(0, maxLen) + "…";
}

// --- ошибки ---

function showError(container, err) {
  clear(container);
  container.appendChild(
    el("div", {
      class: "rounded-md border border-red-300 bg-red-50 px-4 py-3 text-sm text-red-800 dark:border-red-800 dark:bg-red-950 dark:text-red-200",
      text: "Ошибка: " + (err && err.message ? err.message : String(err)),
    })
  );
}

// --- дашборд статистики ---

// statsPromise — общая загрузка /api/stats: дашборд и фильтры списка, а с 15.6
// ещё и селект scope доски (ждёт статистику, чтобы выбрать проект по умолчанию).
let statsPromise = null;

function loadStats() {
  const box = document.getElementById("dashboard");
  statsPromise = (async function () {
    try {
      const stats = await api("/api/stats");
      statsCache = stats;
      renderDashboard(stats);
      renderFilterCheckboxes(stats);
      populateBoardScopeSelect(stats);
    } catch (err) {
      showError(box, err);
    }
  })();
  return statsPromise;
}

function renderDashboard(stats) {
  const box = document.getElementById("dashboard");
  clear(box);

  const counters = el("div", { class: "flex flex-wrap gap-4 text-sm" }, [
    el("div", { class: "rounded-md bg-slate-100 px-3 py-2 dark:bg-slate-800" }, [
      el("span", { class: "font-semibold text-slate-900 dark:text-slate-100", text: String(stats.total) }),
      el("span", { class: "ml-1 text-slate-600 dark:text-slate-400", text: "живых записей" }),
    ]),
    el("div", { class: "rounded-md bg-slate-100 px-3 py-2 dark:bg-slate-800" }, [
      el("span", { class: "font-semibold text-slate-900 dark:text-slate-100", text: String(stats.superseded) }),
      el("span", { class: "ml-1 text-slate-600 dark:text-slate-400", text: "архивных (superseded)" }),
    ]),
  ]);
  box.appendChild(counters);

  const badgeRow = el("div", { class: "mt-3 flex flex-wrap gap-2" });

  Object.keys(stats.byKind).sort().forEach(function (kind) {
    const count = stats.byKind[kind];
    badgeRow.appendChild(
      el(
        "button",
        {
          class: "cursor-pointer rounded-full px-3 py-1 text-xs font-medium " + (KIND_BADGE_CLASS[kind] || KIND_BADGE_CLASS_DEFAULT),
          attrs: { type: "button", title: "фильтр по kind: " + kind },
          onClick: function () {
            toggleFilter(state.kinds, kind);
            renderFilterCheckboxes(statsCache);
            loadList();
          },
        },
        [el("span", { text: (KIND_LABEL[kind] || kind) + " · " + count })]
      )
    );
  });

  Object.keys(stats.byScope).sort().forEach(function (scope) {
    const count = stats.byScope[scope];
    badgeRow.appendChild(
      el(
        "button",
        {
          class: "cursor-pointer rounded-full border border-slate-300 px-3 py-1 text-xs font-medium text-slate-700 dark:border-slate-600 dark:text-slate-300",
          attrs: { type: "button", title: "фильтр по scope: " + scope },
          onClick: function () {
            toggleFilter(state.scopes, scope);
            renderFilterCheckboxes(statsCache);
            loadList();
          },
        },
        [el("span", { text: scope + " · " + count })]
      )
    );
  });

  box.appendChild(badgeRow);
}

function toggleFilter(set, value) {
  if (set.has(value)) set.delete(value);
  else set.add(value);
}

// --- панель фильтров (чекбоксы scope/kind) ---

function renderFilterCheckboxes(stats) {
  if (!stats) return;

  const kindBox = document.getElementById("kind-filters");
  clear(kindBox);
  Object.keys(stats.byKind).sort().forEach(function (kind) {
    kindBox.appendChild(filterCheckbox(kind, KIND_LABEL[kind] || kind, state.kinds));
  });

  const scopeBox = document.getElementById("scope-filters");
  clear(scopeBox);
  Object.keys(stats.byScope).sort().forEach(function (scope) {
    scopeBox.appendChild(filterCheckbox(scope, scope, state.scopes));
  });
}

function filterCheckbox(value, label, set) {
  const id = "flt-" + Math.random().toString(36).slice(2);
  const input = el("input", {
    attrs: { type: "checkbox", id: id },
  });
  input.checked = set.has(value);
  input.addEventListener("change", function () {
    if (input.checked) set.add(value);
    else set.delete(value);
    loadList();
  });
  const labelEl = el("label", { class: "ml-1 cursor-pointer select-none text-sm text-slate-700 dark:text-slate-300", attrs: { for: id }, text: label });
  return el("div", { class: "flex items-center" }, [input, labelEl]);
}

// --- список записей + ленивая подгрузка (infinite scroll) ---

function buildListQuery() {
  const p = new URLSearchParams();
  state.scopes.forEach(function (s) {
    p.append("scope", s);
  });
  state.kinds.forEach(function (k) {
    p.append("kind", k);
  });
  if (state.showSuperseded) p.set("superseded", "1");
  p.set("limit", String(state.limit));
  p.set("offset", String(state.offset));
  return p;
}

// Постоянные узлы статуса/сентинела и наблюдатель создаются один раз
// (setupInfiniteScroll) и живут между перезагрузками списка, чтобы
// IntersectionObserver не терял цель при смене фильтров.
let scrollObserver = null;
let sentinelEl = null;
let statusEl = null;

function setupInfiniteScroll() {
  const pagBox = document.getElementById("pagination");
  const wrap = el("div", { class: "flex w-full flex-col items-center gap-2" });
  statusEl = el("div", { class: "text-sm text-slate-500 dark:text-slate-400" });
  // сентинел: тонкая полоса внизу списка; её появление во вьюпорте (с запасом
  // rootMargin) триггерит подгрузку следующей страницы.
  sentinelEl = el("div", { class: "h-px w-full", attrs: { "aria-hidden": "true" } });
  wrap.appendChild(statusEl);
  wrap.appendChild(sentinelEl);
  pagBox.appendChild(wrap);

  scrollObserver = new IntersectionObserver(
    function (entries) {
      for (let i = 0; i < entries.length; i++) {
        if (entries[i].isIntersecting) {
          fetchPage(false);
          break;
        }
      }
    },
    { root: null, rootMargin: "300px 0px" }
  );
}

// attachSentinel запускает наблюдение (unobserve+observe перезапускает коллбэк,
// если сентинел всё ещё виден — доподгружает, пока не заполнит вьюпорт либо не
// кончатся записи). detachSentinel останавливает подгрузку.
function attachSentinel() {
  if (!scrollObserver) return;
  scrollObserver.unobserve(sentinelEl);
  scrollObserver.observe(sentinelEl);
}

function detachSentinel() {
  if (scrollObserver) scrollObserver.unobserve(sentinelEl);
}

// loadList — точка входа «с нуля»: смена фильтров/поиска/сброс/после удаления.
// Прерывает текущую загрузку, сбрасывает пагинацию и грузит первую страницу.
function loadList() {
  if (state.controller) state.controller.abort();
  state.controller = null;
  state.offset = 0;
  state.total = 0;
  state.exhausted = false;

  clear(document.getElementById("cards-container"));
  clear(document.getElementById("results-summary"));
  detachSentinel();
  if (statusEl) statusEl.textContent = "";
  updateDegradedBanner(null);

  fetchPage(true);
}

// updateDegradedBanner показывает/скрывает плашку «поиск по смыслу недоступен»
// (задача 10.1): видна только когда выбран режим "semantic" и сервер честно
// сообщил vector:false (Embedder недоступен/выключен — гибрид молча деградировал
// до FTS5). vectorAvailable=null используется для сброса баннера вне поиска.
function updateDegradedBanner(vectorAvailable) {
  const banner = document.getElementById("search-degraded-banner");
  if (!banner) return;
  const shouldShow = state.mode === "semantic" && vectorAvailable === false;
  banner.classList.toggle("hidden", !shouldShow);
}

// fetchPage грузит ОДНУ страницу. isReset=true — первая страница (заменяет
// список), иначе — дозагрузка (аппенд). Поиск (`/api/search`) не поддерживает
// offset-пагинацию (контракт 6.2) — грузится одним запросом, без infinite scroll.
async function fetchPage(isReset) {
  const listBox = document.getElementById("cards-container");
  const summaryBox = document.getElementById("results-summary");
  const q = state.query.trim();
  const isSearch = q !== "";

  if (state.controller) return; // загрузка уже идёт
  if (!isReset && (state.exhausted || isSearch)) return;

  const controller = new AbortController();
  state.controller = controller;

  if (isReset) {
    listBox.appendChild(el("div", { class: "text-sm text-slate-500 dark:text-slate-400", text: "загрузка…" }));
  } else if (statusEl) {
    statusEl.textContent = "загрузка ещё…";
  }

  try {
    if (isSearch) {
      const p = buildListQuery();
      p.set("q", q);
      p.set("mode", state.mode);
      p.delete("offset");
      const data = await api("/api/search?" + p.toString(), { signal: controller.signal });
      if (state.controller !== controller) return; // вытеснено новой загрузкой
      clear(listBox);
      renderSearchResults(data.items || [], summaryBox, listBox);
      updateDegradedBanner(data.vector);
      state.exhausted = true;
    } else {
      const p = buildListQuery();
      const data = await api("/api/memories?" + p.toString(), { signal: controller.signal });
      if (state.controller !== controller) return; // вытеснено новой загрузкой
      const items = data.items || [];
      state.total = data.total;
      if (isReset) clear(listBox);
      if (isReset && items.length === 0) {
        listBox.appendChild(emptyState("Записей нет — измени фильтры."));
        state.exhausted = true;
      } else {
        items.forEach(function (item) {
          listBox.appendChild(renderCard(item, false));
        });
      }
      state.offset += items.length;
      if (items.length === 0 || state.offset >= state.total) state.exhausted = true;
      updateBrowseStatus(summaryBox);
    }
  } catch (err) {
    if (err && err.name === "AbortError") return; // вытеснено новой загрузкой
    if (state.controller !== controller) return;
    if (isReset) {
      clear(listBox);
      showError(listBox, err);
    } else {
      // Ошибка дозагрузки: сентинел остаётся в зоне видимости и больше не
      // триггерит observer сам по себе — снимаем его и даём явный ретрай,
      // иначе подгрузка молча залипает (без авто-ретрая, чтобы не долбить
      // упавший эндпоинт в цикле).
      detachSentinel();
      renderLoadMoreError(err);
    }
    return;
  } finally {
    // Сбрасываем только если нас не вытеснила новая загрузка (иначе она уже
    // владеет state.controller и сама им управляет).
    if (state.controller === controller) state.controller = null;
  }

  // Дозагрузка следующей страницы по мере скролла — только в режиме просмотра.
  if (!isSearch && !state.exhausted) attachSentinel();
  else detachSentinel();
}

// emptyState — карточка-заглушка «ничего нет» (общая для пустого просмотра и
// пустого поиска — отличается только текстом).
function emptyState(text) {
  return el("div", {
    class: "rounded-md border border-slate-200 bg-slate-50 px-4 py-6 text-center text-sm text-slate-500 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-400",
    text: text,
  });
}

// renderLoadMoreError — ошибка дозагрузки со строкой-статусом и кнопкой
// повтора (ручной ретрай текущей страницы просмотра).
function renderLoadMoreError(err) {
  if (!statusEl) return;
  clear(statusEl);
  statusEl.appendChild(
    el("span", {
      class: "text-red-700 dark:text-red-400",
      text: "ошибка подгрузки: " + (err && err.message ? err.message : String(err)) + " ",
    })
  );
  statusEl.appendChild(
    el("button", {
      class: BTN_SECONDARY,
      attrs: { type: "button" },
      text: "Повторить",
      onClick: function () {
        clear(statusEl);
        fetchPage(false);
      },
    })
  );
}

// updateBrowseStatus — сводка сверху («показано N из TOTAL») и строка снизу
// («все записи загружены», когда страниц больше нет).
function updateBrowseStatus(summaryBox) {
  summaryBox.textContent = "показано " + state.offset + " из " + state.total;
  if (!statusEl) return;
  statusEl.textContent =
    state.exhausted && state.total > 0 ? "все записи загружены (" + state.total + ")" : "";
}

// renderSearchResults — выдача поиска (без пагинации, см. контракт 6.2).
function renderSearchResults(items, summaryBox, listBox) {
  if (statusEl) statusEl.textContent = "";
  if (items.length === 0) {
    summaryBox.textContent = "";
    listBox.appendChild(emptyState("По запросу ничего не найдено."));
    return;
  }
  items.forEach(function (item) {
    listBox.appendChild(renderCard(item, true));
  });
  // /api/search не отдаёт total и режет выдачу по limit — ровно limit элементов
  // означает вероятную обрезку, а не «найдено всё» (DESIGN.md §15).
  summaryBox.textContent =
    items.length === state.limit
      ? "показаны первые " + items.length + " совпадений (уточни запрос)"
      : "найдено: " + items.length;
}

function renderCard(item, isSearch) {
  const kindBadge = el("span", {
    class: "rounded-full px-2 py-0.5 text-xs font-medium " + (KIND_BADGE_CLASS[item.kind] || KIND_BADGE_CLASS_DEFAULT),
    text: KIND_LABEL[item.kind] || item.kind,
  });
  const scopeBadge = el("span", {
    class: "rounded-full border border-slate-300 px-2 py-0.5 text-xs text-slate-600 dark:border-slate-600 dark:text-slate-400",
    text: item.scope,
  });
  const stars = el("span", { class: "text-amber-500 text-xs", text: fmtStars(item.importance) });

  const topRow = el("div", { class: "flex flex-wrap items-center gap-2" }, [kindBadge, scopeBadge, stars]);

  const status = statusBadge(item.status);
  if (status) topRow.appendChild(status);
  const taskType = taskTypeBadge(item.type);
  if (taskType) topRow.appendChild(taskType);

  if (item.supersededBy) {
    topRow.appendChild(
      el("span", {
        class: "rounded-full bg-slate-200 px-2 py-0.5 text-xs text-slate-600 dark:bg-slate-700 dark:text-slate-300",
        text: "архив",
      })
    );
  }
  if (isSearch) {
    const sourceBadge = el("span", {
      class: "rounded-full border px-1.5 py-0.5 text-xs font-medium " + (SOURCE_BADGE_CLASS[item.source] || SOURCE_BADGE_CLASS_DEFAULT),
      text: SOURCE_LABEL[item.source] || item.source,
    });
    topRow.appendChild(
      el("span", { class: "ml-auto flex items-center gap-2 text-xs text-slate-500 dark:text-slate-400" }, [
        el("span", { text: "score " + item.score.toFixed(3) }),
        sourceBadge,
      ])
    );
  }

  // У задачи (kind=task) summary — заголовок, а content — весь документ задачи:
  // без отдельной строки заголовка список задач по одной подсистеме выглядит
  // как несколько одинаковых абзацев. Для остальных типов памяти карточка
  // остаётся прежней: там summary — не заголовок, а краткий пересказ content,
  // и печатать оба подряд значило бы дублировать одно и то же.
  const titleEl = item.kind === "task" && item.summary
    ? el("p", { class: "mt-2 text-sm font-semibold text-slate-900 dark:text-slate-100", text: item.summary })
    : null;
  const contentEl = el("p", { class: "mt-2 text-sm text-slate-800 dark:text-slate-200", text: firstLines(item.content, 220) });

  const tagsRow = el("div", { class: "mt-2 flex flex-wrap gap-1" });
  (item.tags || []).forEach(function (tag) {
    tagsRow.appendChild(
      el("span", { class: "rounded bg-slate-100 px-1.5 py-0.5 text-xs text-slate-600 dark:bg-slate-700 dark:text-slate-300", text: "#" + tag })
    );
  });

  const dateEl = el("span", { class: "text-xs text-slate-500 dark:text-slate-400", text: fmtDate(item.createdAt) });

  const card = el(
    "div",
    {
      class:
        "cursor-pointer rounded-lg border border-slate-200 bg-white p-4 shadow-sm transition hover:shadow-md dark:border-slate-700 dark:bg-slate-800",
      onClick: function () {
        openCard(item.id);
      },
    },
    [topRow, titleEl, contentEl, tagsRow, el("div", { class: "mt-2" }, [dateEl])]
  );
  return card;
}

// --- карточка записи (модалка) с навигацией по графу связей ---

async function openCard(id) {
  const backdrop = document.getElementById("modal-backdrop");
  const content = document.getElementById("modal-content");
  clear(content);
  content.appendChild(el("div", { class: "text-sm text-slate-500 dark:text-slate-400", text: "загрузка…" }));
  backdrop.classList.remove("hidden");

  try {
    const card = await api("/api/memories/" + encodeURIComponent(id));
    renderCardModal(card);
  } catch (err) {
    clear(content);
    showError(content, err);
  }
}

function closeModal() {
  document.getElementById("modal-backdrop").classList.add("hidden");
}

// vectorStatusBadge — бейдж состояния векторного слоя записи (задача 6.4).
// embedding может отсутствовать (старый API/деградация) — трактуем как выключенный слой.
function vectorStatusBadge(embedding) {
  const e = embedding || { embedded: false, model: "", dim: 0 };
  if (e.embedded) {
    return el("span", {
      class: "rounded-full border px-2 py-0.5 text-xs text-emerald-700 border-emerald-300 dark:text-emerald-400 dark:border-emerald-700",
      text: "вектор ✓ " + e.model + "·" + e.dim,
    });
  }
  return el("span", {
    class: "rounded-full border border-slate-300 px-2 py-0.5 text-xs text-slate-600 dark:border-slate-600 dark:text-slate-400",
    text: e.model ? "вектор: в очереди" : "векторный слой выключен",
  });
}

// renderDeleteControl — кнопка мягкого удаления записи с двухшаговым
// инлайн-подтверждением (без нативного confirm, чтобы стиль совпадал с
// остальной модалкой). Первый клик заменяет кнопку на «Точно удалить? Да/Отмена».
function renderDeleteControl(id) {
  const box = el("div", { class: "mt-4 border-t border-slate-200 pt-3 dark:border-slate-700" });
  const errorBox = el("div", { class: "mb-2" });

  function renderIdle() {
    clear(box);
    clear(errorBox);
    box.appendChild(errorBox);
    box.appendChild(
      el(
        "button",
        {
          class: BTN_DANGER,
          attrs: {
            type: "button",
            title: "мягкое удаление: запись уходит в архив (superseded), история сохраняется",
          },
          onClick: renderConfirm,
        },
        [icon(TRASH_PATH, "-ml-0.5 size-4"), el("span", { text: "Удалить (в архив)" })]
      )
    );
  }

  function renderConfirm() {
    clear(box);
    box.appendChild(errorBox);

    const yesBtn = el(
      "button",
      {
        class: BTN_DANGER,
        attrs: { type: "button" },
        onClick: async function () {
          yesBtn.disabled = true;
          cancelBtn.disabled = true;
          try {
            await apiDelete("/api/memories/" + encodeURIComponent(id));
            closeModal();
            loadStats();
            refreshCurrentView();
          } catch (err) {
            showError(errorBox, err);
            yesBtn.disabled = false;
            cancelBtn.disabled = false;
          }
        },
      },
      [icon(TRASH_PATH, "-ml-0.5 size-4"), el("span", { text: "Да, удалить" })]
    );
    const cancelBtn = el(
      "button",
      {
        class: BTN_SECONDARY,
        attrs: { type: "button" },
        text: "Отмена",
        onClick: renderIdle,
      },
      []
    );

    box.appendChild(
      el("div", { class: "flex items-center gap-2" }, [
        el("span", { class: "text-sm text-red-700 dark:text-red-400", text: "Точно удалить?" }),
        yesBtn,
        cancelBtn,
      ])
    );
  }

  renderIdle();
  return box;
}

// renderImportanceControl — пять звёзд-кнопок (задача 10.2): текущее значение
// подсвечено (заполненные звёзды), клик по звезде N сразу шлёт PATCH
// {importance:N} (одно значение за раз, без отдельной кнопки «сохранить» —
// в отличие от тегов ниже, здесь нечего копить). Пока запрос в полёте, звёзды
// блокируются (нет двойной отправки); ошибка — плашка под звёздами, а
// отображённое значение откатывается к последнему серверному (prev).
// onUpdated получает свежий memoryDTO из ответа сервера.
function renderImportanceControl(id, importance, onUpdated) {
  const box = el("div", { class: "flex items-center gap-2" });
  const starsRow = el("div", { class: "flex items-center gap-0.5" });
  const errorBox = el("div", {});
  let current = importance;
  let busy = false;

  function render() {
    clear(starsRow);
    for (let i = 1; i <= 5; i++) {
      const filled = i <= current;
      const n = i; // фиксируем значение для замыкания onClick
      starsRow.appendChild(
        el(
          "button",
          {
            class:
              "text-lg leading-none " +
              (filled ? "text-amber-500" : "text-slate-300 dark:text-slate-600") +
              (busy ? " opacity-50" : " hover:text-amber-400"),
            attrs: busy
              ? { type: "button", disabled: "disabled", title: "важность " + n }
              : { type: "button", title: "важность " + n, "aria-label": "важность " + n },
            onClick: busy ? null : function () { submit(n); },
            text: filled ? "★" : "☆",
          },
          []
        )
      );
    }
  }

  async function submit(next) {
    if (busy || next === current) return;
    const prev = current;
    busy = true;
    current = next;
    clear(errorBox);
    render();
    try {
      const updated = await apiPatch("/api/memories/" + encodeURIComponent(id), { importance: next });
      busy = false;
      current = updated.importance;
      render();
      onUpdated(updated);
    } catch (err) {
      busy = false;
      current = prev; // откат к серверному значению
      render();
      showError(errorBox, err);
    }
  }

  render();
  box.appendChild(starsRow);
  box.appendChild(errorBox);
  return box;
}

// renderTagsControl — редактирование тегов (задача 10.2): чипы с крестиком
// удаления, поле ввода (Enter добавляет чип локально, без запроса), кнопка
// «Сохранить теги» шлёт ОДИН PATCH с итоговым списком целиком (не запрос на
// каждый чип — см. критерий задачи). Ошибка — плашка, черновик (draft)
// откатывается к последним серверным тегам.
function renderTagsControl(id, tags, onUpdated) {
  const box = el("div", { class: "mt-1" });
  const chipsRow = el("div", { class: "flex flex-wrap gap-1" });
  const errorBox = el("div", { class: "mt-1" });
  const input = el("input", {
    class:
      "rounded border border-slate-300 px-2 py-1 text-xs focus:border-blue-500 focus:outline-none dark:border-slate-600 dark:bg-slate-900 dark:text-slate-100",
    attrs: { type: "text", placeholder: "новый тег + Enter" },
  });
  let serverTags = tags.slice();
  let draft = tags.slice();
  let busy = false;

  function isDirty() {
    if (draft.length !== serverTags.length) return true;
    for (let i = 0; i < draft.length; i++) {
      if (draft[i] !== serverTags[i]) return true;
    }
    return false;
  }

  function renderChips() {
    clear(chipsRow);
    draft.forEach(function (tag, idx) {
      chipsRow.appendChild(
        el(
          "span",
          {
            class:
              "inline-flex items-center gap-1 rounded bg-slate-100 px-1.5 py-0.5 text-xs text-slate-600 dark:bg-slate-700 dark:text-slate-300",
          },
          [
            el("span", { text: "#" + tag }),
            el("button", {
              class: "text-slate-400 hover:text-red-600 dark:hover:text-red-400",
              attrs: { type: "button", title: "убрать тег", "aria-label": "убрать тег " + tag },
              text: "×",
              onClick: busy
                ? null
                : function () {
                    draft.splice(idx, 1);
                    renderAll();
                  },
            }),
          ]
        )
      );
    });
  }

  function addTag() {
    const v = input.value.trim();
    input.value = "";
    if (v === "" || draft.indexOf(v) !== -1) return;
    draft.push(v);
    renderAll();
  }

  const saveBtn = el("button", {
    class: BTN_SECONDARY,
    attrs: { type: "button" },
    text: "Сохранить теги",
    onClick: async function () {
      busy = true;
      renderAll();
      clear(errorBox);
      try {
        const updated = await apiPatch("/api/memories/" + encodeURIComponent(id), { tags: draft });
        busy = false;
        serverTags = (updated.tags || []).slice();
        draft = serverTags.slice();
        renderAll();
        onUpdated(updated);
      } catch (err) {
        busy = false;
        draft = serverTags.slice(); // откат к последним серверным тегам
        renderAll();
        showError(errorBox, err);
      }
    },
  });

  function renderAll() {
    renderChips();
    saveBtn.disabled = busy || !isDirty();
  }

  input.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter") {
      ev.preventDefault();
      addTag();
    }
  });

  box.appendChild(chipsRow);
  box.appendChild(el("div", { class: "mt-1 flex items-center gap-2" }, [input, saveBtn]));
  box.appendChild(errorBox);
  renderAll();
  return box;
}

function renderCardModal(card) {
  const content = document.getElementById("modal-content");
  clear(content);
  const m = card.memory;
  const isLive = !m.supersededBy;

  // handleMetadataUpdated — общий колбэк для звёзд/тегов (задача 10.2): PATCH
  // возвращает обновлённый memoryDTO, сливаем его в card.memory и перерисовываем
  // карточку целиком — простая и надёжная синхронизация, не нужно вручную
  // разводить, какие DOM-узлы затронуты правкой importance/tags/updatedAt.
  function handleMetadataUpdated(updated) {
    Object.assign(card.memory, updated);
    renderCardModal(card);
    refreshBoardIfActive();
  }

  // reloadCard — перечитать карточку целиком после PATCH, меняющего то, что
  // считает board.Describe (стадия, родитель, блокеры): parent/children/
  // blockedBy/blocks в memoryDTO нет, слить ответ PATCH недостаточно. Если
  // открыта доска — обновить и её (карточка на доске тоже изменилась).
  async function reloadCard() {
    try {
      const fresh = await api("/api/memories/" + encodeURIComponent(m.id));
      renderCardModal(fresh);
    } catch (err) {
      showToast("не удалось обновить карточку: " + errMessage(err));
    }
    refreshBoardIfActive();
  }

  const kindBadge = el("span", {
    class: "rounded-full px-2 py-0.5 text-xs font-medium " + (KIND_BADGE_CLASS[m.kind] || KIND_BADGE_CLASS_DEFAULT),
    text: KIND_LABEL[m.kind] || m.kind,
  });
  const scopeBadge = el("span", {
    class: "rounded-full border border-slate-300 px-2 py-0.5 text-xs text-slate-600 dark:border-slate-600 dark:text-slate-400",
    text: m.scope,
  });
  // Живая запись — интерактивные звёзды (клик = PATCH importance, задача 10.2);
  // архивную (тумстоун) курировать нечего — только текст (сервер вернёт 409).
  const importanceEl = isLive
    ? renderImportanceControl(m.id, m.importance, handleMetadataUpdated)
    : el("span", { class: "text-amber-500 text-sm", text: fmtStars(m.importance) });

  const badges = [kindBadge, scopeBadge, importanceEl, vectorStatusBadge(card.embedding)];
  const statusEl = statusBadge(m.status);
  if (statusEl) badges.splice(2, 0, statusEl);
  const taskTypeEl = taskTypeBadge(m.type);
  if (taskTypeEl) badges.splice(statusEl ? 3 : 2, 0, taskTypeEl);
  content.appendChild(el("div", { class: "flex flex-wrap items-center gap-2" }, badges));

  if (m.supersededBy) {
    content.appendChild(
      el("p", {
        class: "mt-2 text-sm text-amber-700 dark:text-amber-400",
        text: "Архивная запись — заменена записью " + m.supersededBy,
      })
    );
  }

  content.appendChild(el("h3", { class: "mt-3 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Содержимое" }));
  content.appendChild(el("p", { class: "mt-1 whitespace-pre-wrap text-sm text-slate-900 dark:text-slate-100", text: m.content }));

  if (m.summary) {
    content.appendChild(el("h3", { class: "mt-3 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Сводка" }));
    content.appendChild(el("p", { class: "mt-1 text-sm text-slate-800 dark:text-slate-200", text: m.summary }));
  }

  // --- рабочие метаданные задачи (kind=task, задача 15.6): стадия, тип,
  // ветка, workflow — редактируются у живой задачи; у архивной — только бейджи
  // выше. Для прочих kind полей нет — секция не рисуется. ---
  const isTask = m.kind === "task";
  if (isTask && isLive) renderTaskFieldsSection(content, m, reloadCard);

  // --- иерархия задачи (kind=task, задача 15.3/15.6): родитель по part_of
  // (у живой задачи — с пикером и снятием) и дети с прогрессом (read-only,
  // клик открывает карточку). Для не-задач сервер отдаёт parent/progress = null
  // и пустой children — секция не рисуется. ---
  renderHierarchySection(content, card, isTask && isLive, reloadCard);

  // --- блокировки (задача 15.4/15.6): «ждёт» — редактируемый список блокеров,
  // «блокирует» — read-only обратная сторона. ---
  if (isTask) renderBlockingSections(content, card, isLive, reloadCard);

  content.appendChild(el("h3", { class: "mt-3 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Теги" }));
  if (isLive) {
    // Живая запись — редактируемые чипы (добавить/убрать, «Сохранить теги» =
    // один PATCH с итоговым списком, задача 10.2).
    content.appendChild(renderTagsControl(m.id, m.tags || [], handleMetadataUpdated));
  } else if (m.tags && m.tags.length > 0) {
    const tagsRow = el("div", { class: "mt-1 flex flex-wrap gap-1" });
    m.tags.forEach(function (tag) {
      tagsRow.appendChild(el("span", { class: "rounded bg-slate-100 px-1.5 py-0.5 text-xs text-slate-600 dark:bg-slate-700 dark:text-slate-300", text: "#" + tag }));
    });
    content.appendChild(tagsRow);
  } else {
    content.appendChild(el("p", { class: "mt-1 text-sm text-slate-500 dark:text-slate-400", text: "нет тегов" }));
  }

  const metaList = el("dl", { class: "mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-slate-600 dark:text-slate-400" }, [
    el("dt", { text: "создано" }),
    el("dd", { text: fmtDate(m.createdAt) }),
    el("dt", { text: "изменено" }),
    el("dd", { text: fmtDate(m.updatedAt) }),
    el("dt", { text: "последний доступ" }),
    el("dd", { text: fmtDate(m.accessedAt) }),
    el("dt", { text: "обращений" }),
    el("dd", { text: String(m.accessCount) }),
    el("dt", { text: "источник" }),
    el("dd", { text: m.source || "—" }),
    el("dt", { text: "id" }),
    el("dd", { text: m.id }),
  ]);
  content.appendChild(metaList);

  // --- удаление (мягкое, в архив) — только для живых записей; архивную
  // архивировать повторно нечего ---
  if (!m.supersededBy) {
    content.appendChild(renderDeleteControl(m.id));
  }

  // --- граф связей: links (исходящие, с типом отношения) + neighbors (полные карточки соседей) ---
  if (m.links && m.links.length > 0) {
    content.appendChild(el("h3", { class: "mt-4 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Связи" }));
    const linksList = el("ul", { class: "mt-1 space-y-1" });
    m.links.forEach(function (link) {
      const li = el("li", {}, [
        el(
          "button",
          {
            class: "text-sm text-blue-600 underline hover:text-blue-800 dark:text-blue-400",
            attrs: { type: "button" },
            text: link.rel + " → " + link.dst,
            onClick: function () {
              openCard(link.dst);
            },
          },
          []
        ),
      ]);
      linksList.appendChild(li);
    });
    content.appendChild(linksList);
  }

  if (card.neighbors && card.neighbors.length > 0) {
    content.appendChild(el("h3", { class: "mt-4 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Соседи по графу" }));
    const neighborsBox = el("div", { class: "mt-1 space-y-2" });
    card.neighbors.forEach(function (n) {
      neighborsBox.appendChild(renderCard(n, false));
    });
    content.appendChild(neighborsBox);
  }

  // --- семантические соседи по вектору (KNN cosine), задача 6.4 ---
  content.appendChild(el("h3", { class: "mt-4 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Семантически близкие (по вектору)" }));
  if (card.semanticNeighbors && card.semanticNeighbors.length > 0) {
    const semanticBox = el("div", { class: "mt-1 space-y-2" });
    card.semanticNeighbors.forEach(function (n) {
      semanticBox.appendChild(
        el("div", {}, [
          el("div", { class: "text-xs text-slate-500 dark:text-slate-400", text: "дистанция " + n.distance.toFixed(2) }),
          renderCard(n, false),
        ])
      );
    });
    content.appendChild(semanticBox);
  } else {
    content.appendChild(
      el("p", {
        class: "mt-1 text-sm text-slate-500 dark:text-slate-400",
        text: "нет семантических соседей / векторный слой выключен",
      })
    );
  }
}

// renderTaskRef — строка задачи в секциях иерархии: бейджи стадии/типа + заголовок
// (summary, иначе id) кнопкой, открывающей карточку.
function renderTaskRef(ref) {
  const badges = [statusBadge(ref.status), taskTypeBadge(ref.type)];
  return el("div", { class: "flex flex-wrap items-center gap-2" }, [
    el("div", { class: "flex items-center gap-1" }, badges),
    el("button", {
      class: "text-sm text-blue-600 underline hover:text-blue-800 dark:text-blue-400",
      attrs: { type: "button" },
      text: ref.summary || ref.id,
      onClick: function () {
        openCard(ref.id);
      },
    }),
  ]);
}

// renderHierarchySection добавляет в content секции «Родитель» и «Дети (n, готово k)»
// карточки задачи; пустые секции не рисуются. editable — живая задача: родителя
// можно снять (unlinks part_of) и выбрать/заменить пикером (links part_of;
// замена — старое ребро снимается тем же PATCH: Update применяет links, затем
// unlinks). onChanged — перечитать карточку после успешного PATCH.
function renderHierarchySection(content, card, editable, onChanged) {
  const m = card.memory;
  if (card.parent || editable) {
    content.appendChild(el("h3", { class: "mt-3 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Родитель" }));
  }
  if (card.parent) {
    const row = el("div", { class: "mt-1 flex flex-wrap items-center gap-2" }, [renderTaskRef(card.parent)]);
    if (editable) {
      row.appendChild(
        renderUnlinkButton("снять родителя", function () {
          return apiPatch("/api/memories/" + encodeURIComponent(m.id), { unlinks: [{ id: card.parent.id, rel: "part_of" }] });
        }, onChanged)
      );
    }
    content.appendChild(row);
  }
  if (editable) {
    content.appendChild(
      renderTaskPicker(card.parent ? "заменить родителя…" : "выбрать родителя (эпик/историю)…", [m.id], function (picked) {
        const body = { links: [{ id: picked.id, rel: "part_of" }] };
        if (card.parent && card.parent.id !== picked.id) body.unlinks = [{ id: card.parent.id, rel: "part_of" }];
        return apiPatch("/api/memories/" + encodeURIComponent(m.id), body);
      }, onChanged)
    );
  }
  const children = card.children || [];
  if (children.length > 0) {
    const progress = card.progress || { done: 0, total: children.length };
    content.appendChild(
      el("h3", {
        class: "mt-3 text-sm font-semibold text-slate-500 dark:text-slate-400",
        text: "Дети (" + progress.total + ", готово " + progress.done + ")",
      })
    );
    const list = el("div", { class: "mt-1 space-y-1" });
    children.forEach(function (c) {
      list.appendChild(renderTaskRef(c));
    });
    content.appendChild(list);
  }
}

// --- тосты (задача 15.6): один общий компонент для ошибок вне модалки —
// отказ переноса карточки (409 с блокерами / 400), сбой пикера. Контейнер
// #toasts фиксирован в углу; тост исчезает сам через 8 с или по крестику. ---

function errMessage(err) {
  return err && err.message ? err.message : String(err);
}

function showToast(text, kind) {
  const box = document.getElementById("toasts");
  if (!box) return;
  const isInfo = kind === "info";
  const toast = el(
    "div",
    {
      class:
        "pointer-events-auto flex items-start gap-2 rounded-md border px-3 py-2 text-sm shadow-lg " +
        (isInfo
          ? "border-slate-300 bg-white text-slate-800 dark:border-slate-600 dark:bg-slate-800 dark:text-slate-100"
          : "border-red-300 bg-red-50 text-red-800 dark:border-red-800 dark:bg-red-950 dark:text-red-200"),
      attrs: { role: isInfo ? "status" : "alert" },
    },
    [
      el("span", { class: "flex-1 whitespace-pre-wrap", text: text }),
      el("button", {
        class: "shrink-0 opacity-70 hover:opacity-100",
        attrs: { type: "button", "aria-label": "закрыть" },
        text: "✕",
        onClick: function () {
          dismiss();
        },
      }),
    ]
  );
  let timer = setTimeout(dismiss, 8000);
  function dismiss() {
    clearTimeout(timer);
    if (toast.parentNode) toast.parentNode.removeChild(toast);
  }
  box.appendChild(toast);
}

// blockedErrorText — текст ошибки PATCH с перечнем блокеров из тела 409
// ({error, blockers:[{id, summary, status, type}]}); для прочих ошибок — message.
function blockedErrorText(err) {
  const blockers = err && err.body && Array.isArray(err.body.blockers) ? err.body.blockers : null;
  if (!blockers || blockers.length === 0) return errMessage(err);
  const names = blockers.map(function (b) {
    return "«" + (b.summary || b.id) + "» (" + (STATUS_LABEL[b.status] || b.status) + ")";
  });
  return errMessage(err) + "\nждёт: " + names.join(", ");
}

// --- контролы задачи в модалке (задача 15.6) ---

const FIELD_LABEL_CLASS = "block text-xs font-semibold uppercase text-slate-500 dark:text-slate-400";
// Без ширины/отступов: их добавляет вызывающий код (w-full в модалке, w-36 в
// фильтрах доски) — две width-утилиты в одном class-атрибуте конфликтовали бы.
const SELECT_CLASS =
  "rounded border border-slate-300 bg-white px-2 py-1 text-sm focus:border-blue-500 focus:outline-none disabled:opacity-50 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-100";
const INPUT_CLASS =
  "rounded border border-slate-300 px-2 py-1 text-sm focus:border-blue-500 focus:outline-none disabled:opacity-50 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-100";

// renderSelectControl — селект с немедленным PATCH при смене (одно значение,
// копить нечего — как у звёзд importance). Ошибка — откат к серверному
// значению + тост (409 блокировки — с именами блокеров). onChanged — после
// успеха (перечитать карточку).
function renderSelectControl(labelText, value, options, submit, onChanged) {
  const select = el("select", { class: SELECT_CLASS + " mt-1 w-full" });
  options.forEach(function (opt) {
    const o = el("option", { attrs: { value: opt[0] }, text: opt[1] });
    if (opt[0] === value) o.setAttribute("selected", "selected");
    select.appendChild(o);
  });
  if (options.every(function (opt) { return opt[0] !== value; }) && value) {
    // неизвестное значение (данные более нового бинаря) — показываем как есть
    const o = el("option", { attrs: { value: value, selected: "selected" }, text: value });
    select.appendChild(o);
  }
  select.addEventListener("change", async function () {
    const next = select.value;
    if (next === value) return;
    select.disabled = true;
    try {
      await submit(next);
      onChanged();
    } catch (err) {
      select.value = value;
      select.disabled = false;
      showToast(blockedErrorText(err));
    }
  });
  return el("label", { class: "block" }, [el("span", { class: FIELD_LABEL_CLASS, text: labelText }), select]);
}

// renderTextControl — текстовое поле с кнопкой «Сохранить» (по образцу тегов:
// черновик локально, один PATCH по кнопке/Enter; ошибка — откат к серверному
// значению + плашка под полем). Пустая строка — валидное «снять значение».
function renderTextControl(labelText, value, placeholder, submit, onChanged) {
  const input = el("input", { class: INPUT_CLASS + " w-full", attrs: { type: "text", placeholder: placeholder } });
  input.value = value || "";
  const errorBox = el("div", { class: "mt-1" });
  let busy = false;
  const saveBtn = el("button", { class: BTN_SECONDARY, attrs: { type: "button" }, text: "Сохранить" });
  function sync() {
    saveBtn.disabled = busy || input.value.trim() === (value || "");
    input.disabled = busy;
  }
  async function save() {
    if (saveBtn.disabled) return;
    busy = true;
    sync();
    clear(errorBox);
    try {
      await submit(input.value.trim());
      onChanged();
    } catch (err) {
      busy = false;
      input.value = value || "";
      sync();
      showError(errorBox, err);
    }
  }
  input.addEventListener("input", sync);
  input.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter") {
      ev.preventDefault();
      save();
    }
  });
  saveBtn.addEventListener("click", save);
  sync();
  return el("div", {}, [
    el("label", { class: "block" }, [
      el("span", { class: FIELD_LABEL_CLASS, text: labelText }),
      el("div", { class: "mt-1 flex items-center gap-2" }, [input, saveBtn]),
    ]),
    errorBox,
  ]);
}

// renderTaskFieldsSection — секция «Задача» модалки: стадия/тип (селекты) и
// ветка/workflow (текст). Каждый контрол шлёт свой PATCH одним полем; после
// успеха карточка перечитывается (onChanged) — смена стадии меняет и то, что
// считает board.Describe.
function renderTaskFieldsSection(content, m, onChanged) {
  const path = "/api/memories/" + encodeURIComponent(m.id);
  content.appendChild(el("h3", { class: "mt-3 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Задача" }));
  content.appendChild(
    el("div", { class: "mt-1 grid grid-cols-1 gap-3 sm:grid-cols-2" }, [
      renderSelectControl(
        "стадия",
        m.status,
        STATUSES.map(function (st) { return [st, STATUS_LABEL[st]]; }),
        function (next) { return apiPatch(path, { status: next }); },
        onChanged
      ),
      renderSelectControl(
        "тип",
        m.type || "task",
        TYPES.map(function (t) { return [t, TYPE_LABEL[t]]; }),
        function (next) { return apiPatch(path, { type: next }); },
        onChanged
      ),
      renderTextControl("ветка", m.branch, "feat/…", function (next) { return apiPatch(path, { branch: next }); }, onChanged),
      renderTextControl("workflow", m.workflow, "имя workflow", function (next) { return apiPatch(path, { workflow: next }); }, onChanged),
    ])
  );
}

// renderUnlinkButton — маленькая кнопка снятия связи (родитель/блокер): один
// PATCH {unlinks}, на время запроса блокируется, ошибка — тост.
function renderUnlinkButton(title, doPatch, onChanged) {
  const btn = el("button", {
    class: "text-slate-400 hover:text-red-600 dark:hover:text-red-400",
    attrs: { type: "button", title: title, "aria-label": title },
    text: "×",
  });
  btn.addEventListener("click", async function () {
    btn.disabled = true;
    try {
      await doPatch();
      onChanged();
    } catch (err) {
      btn.disabled = false;
      showToast(blockedErrorText(err));
    }
  });
  return btn;
}

// renderTaskPicker — поиск задачи для выбора родителя/блокера: поле ввода,
// запрос GET /api/search?q=…&mode=lexical&kind=task (отдельного эндпоинта нет,
// DESIGN.md §12) с дебаунсом и вытеснением устаревших ответов; клик по
// результату — doPatch(picked) → onChanged. excludeIds — сама запись и уже
// связанные (домен self-петлю и дубли отсечёт и без нас, но показывать их
// бессмысленно).
function renderTaskPicker(placeholder, excludeIds, doPatch, onChanged) {
  const input = el("input", { class: INPUT_CLASS + " mt-2 w-full", attrs: { type: "search", placeholder: placeholder } });
  const results = el("div", { class: "mt-1 space-y-1" });
  let timer = null;
  let controller = null;

  async function search() {
    const q = input.value.trim();
    if (controller) controller.abort();
    controller = null;
    clear(results);
    if (q === "") return;
    const c = new AbortController();
    controller = c;
    const p = new URLSearchParams();
    p.set("q", q);
    p.set("mode", "lexical");
    p.set("kind", "task");
    p.set("limit", "10");
    try {
      const data = await api("/api/search?" + p.toString(), { signal: c.signal });
      if (controller !== c) return;
      renderResults(data.items || []);
    } catch (err) {
      if (err && err.name === "AbortError") return;
      if (controller !== c) return;
      showError(results, err);
    } finally {
      if (controller === c) controller = null;
    }
  }

  function renderResults(items) {
    clear(results);
    const visible = items.filter(function (it) { return excludeIds.indexOf(it.id) === -1; });
    if (visible.length === 0) {
      results.appendChild(el("div", { class: "text-xs text-slate-500 dark:text-slate-400", text: "ничего не найдено" }));
      return;
    }
    visible.forEach(function (it) {
      const row = el(
        "button",
        {
          class:
            "flex w-full flex-wrap items-center gap-2 rounded border border-slate-200 px-2 py-1 text-left text-sm hover:bg-slate-100 disabled:opacity-50 dark:border-slate-700 dark:hover:bg-slate-700",
          attrs: { type: "button" },
        },
        [
          statusBadge(it.status),
          taskTypeBadge(it.type),
          el("span", { class: "text-slate-900 dark:text-slate-100", text: it.summary || it.id }),
          el("span", { class: "ml-auto text-xs text-slate-500 dark:text-slate-400", text: it.scope }),
        ]
      );
      row.addEventListener("click", async function () {
        row.disabled = true;
        try {
          await doPatch({ id: it.id, summary: it.summary, status: it.status, type: it.type });
          input.value = "";
          clear(results);
          onChanged();
        } catch (err) {
          row.disabled = false;
          showToast(blockedErrorText(err));
        }
      });
      results.appendChild(row);
    });
  }

  input.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(search, 300);
  });
  input.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter") {
      ev.preventDefault();
      clearTimeout(timer);
      search();
    }
  });
  return el("div", {}, [input, results]);
}

// renderBlockingSections — «Заблокирована (ждёт)»: живые блокеры с кнопкой
// снятия (unlinks blocked_by) и пикером добавления (links blocked_by); у
// архивной задачи — только список. «Блокирует» — read-only обратная сторона
// (board.Describe). Пустые read-only секции не рисуются.
function renderBlockingSections(content, card, editable, onChanged) {
  const m = card.memory;
  const path = "/api/memories/" + encodeURIComponent(m.id);
  const blockedBy = card.blockedBy || [];
  const blocks = card.blocks || [];

  if (blockedBy.length > 0 || editable) {
    content.appendChild(
      el("h3", {
        class: "mt-3 text-sm font-semibold " + (blockedBy.length > 0 ? "text-red-700 dark:text-red-400" : "text-slate-500 dark:text-slate-400"),
        text: "Заблокирована (ждёт)" + (blockedBy.length > 0 ? " · " + blockedBy.length : ""),
      })
    );
    const list = el("div", { class: "mt-1 space-y-1" });
    blockedBy.forEach(function (b) {
      const row = el("div", { class: "flex flex-wrap items-center gap-2" }, [renderTaskRef(b)]);
      if (editable) {
        row.appendChild(
          renderUnlinkButton("снять блокировку", function () {
            return apiPatch(path, { unlinks: [{ id: b.id, rel: "blocked_by" }] });
          }, onChanged)
        );
      }
      list.appendChild(row);
    });
    if (blockedBy.length === 0) {
      list.appendChild(el("p", { class: "text-sm text-slate-500 dark:text-slate-400", text: "блокеров нет" }));
    }
    content.appendChild(list);
    if (editable) {
      const exclude = [m.id].concat(blockedBy.map(function (b) { return b.id; }));
      content.appendChild(
        renderTaskPicker("добавить блокер…", exclude, function (picked) {
          return apiPatch(path, { links: [{ id: picked.id, rel: "blocked_by" }] });
        }, onChanged)
      );
    }
  }

  if (blocks.length > 0) {
    content.appendChild(el("h3", { class: "mt-3 text-sm font-semibold text-slate-500 dark:text-slate-400", text: "Блокирует · " + blocks.length }));
    const list = el("div", { class: "mt-1 space-y-1" });
    blocks.forEach(function (b) {
      list.appendChild(renderTaskRef(b));
    });
    content.appendChild(list);
  }
}

// --- hash-роутер (задача 15.6): "#/" — память (список), "#/board?…" — доска.
// Параметры доски живут только в хэше: applyRoute разбирает их в state.board и
// грузит доску; смена фильтра в панели пишет новый хэш (navigateBoard) и
// возвращается сюда через hashchange — одна точка загрузки, ссылка
// воспроизводит экран. ---

function parseRoute() {
  const hash = location.hash || "#/";
  const path = hash.replace(/^#/, "");
  const qIdx = path.indexOf("?");
  const route = qIdx === -1 ? path : path.slice(0, qIdx);
  const params = new URLSearchParams(qIdx === -1 ? "" : path.slice(qIdx + 1));
  return { view: route === "/board" ? "board" : "list", params: params };
}

// boardHash — сериализация state.board в "#/board?…" (только непустые параметры;
// порядок фиксирован, чтобы одинаковое состояние давало одинаковую строку).
function boardHash() {
  const b = state.board;
  const p = new URLSearchParams();
  if (b.scope) p.set("scope", b.scope);
  TYPES.forEach(function (t) {
    if (b.types.has(t)) p.append("type", t);
  });
  if (b.parent) p.set("parent", b.parent);
  if (b.branch) p.set("branch", b.branch);
  if (b.workflow) p.set("workflow", b.workflow);
  if (b.tag) p.set("tag", b.tag);
  if (b.doneLimit > 0) p.set("doneLimit", String(b.doneLimit));
  if (b.groupByEpic) p.set("group", "epic");
  const q = p.toString();
  return "#/board" + (q ? "?" + q : "");
}

// readBoardParams — обратная операция: хэш → state.board (данные/контроллер
// не трогаем). Неизвестные типы отбрасываются молча — сервер ответил бы 400.
function readBoardParams(params) {
  const b = state.board;
  const prevParent = b.parent;
  b.scope = params.get("scope") || "";
  b.types = new Set(params.getAll("type").filter(function (t) { return TYPES.indexOf(t) !== -1; }));
  b.parent = (params.get("parent") || "").trim();
  if (b.parent !== prevParent) b.parentRef = null;
  b.branch = (params.get("branch") || "").trim();
  b.workflow = (params.get("workflow") || "").trim();
  b.tag = (params.get("tag") || "").trim();
  const dl = parseInt(params.get("doneLimit") || "0", 10);
  b.doneLimit = isNaN(dl) || dl < 0 ? 0 : dl;
  b.groupByEpic = params.get("group") === "epic";
}

// navigateBoard — применить изменённое state.board: записать хэш (hashchange
// сделает загрузку); если хэш уже такой (напр. повтор «показать ещё» без
// изменений) — грузим напрямую.
function navigateBoard() {
  const h = boardHash();
  if (location.hash === h) loadBoard();
  else location.hash = h;
}

// defaultBoardScope — проект по умолчанию для доски: первый (по алфавиту)
// project:* из статистики, иначе первый scope вообще, иначе "" (задач нет).
function defaultBoardScope(stats) {
  if (!stats || !stats.byScope) return "";
  const scopes = Object.keys(stats.byScope).sort();
  for (let i = 0; i < scopes.length; i++) {
    if (scopes[i].indexOf("project:") === 0) return scopes[i];
  }
  return scopes.length > 0 ? scopes[0] : "";
}

async function applyRoute() {
  const r = parseRoute();
  state.view = r.view;

  document.getElementById("list-view").classList.toggle("hidden", r.view !== "list");
  document.getElementById("board-view").classList.toggle("hidden", r.view !== "board");
  // доске нужна ширина под 7 колонок; список остаётся узким, как раньше
  const page = document.getElementById("page");
  page.classList.toggle("max-w-5xl", r.view === "list");
  page.classList.toggle("max-w-[1600px]", r.view === "board");
  document.querySelectorAll("#view-nav a[data-view]").forEach(function (a) {
    const active = a.getAttribute("data-view") === r.view;
    a.classList.toggle("bg-blue-600", active);
    a.classList.toggle("text-white", active);
    a.classList.toggle("hover:bg-slate-100", !active);
    a.classList.toggle("dark:hover:bg-slate-700", !active);
    if (active) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
  closeModal();

  if (r.view === "list") {
    if (!state.listLoaded) {
      state.listLoaded = true;
      loadList();
    }
    return;
  }

  readBoardParams(r.params);
  if (!state.board.scope) {
    // scope обязателен: ждём статистику и подставляем проект по умолчанию,
    // переписывая хэш без записи в историю (replace) — hashchange вернёт нас
    // сюда уже с scope и загрузит доску один раз.
    if (statsPromise) await statsPromise;
    const def = defaultBoardScope(statsCache);
    if (def) {
      state.board.scope = def;
      location.replace(boardHash());
      return;
    }
  }
  syncBoardControls();
  loadBoard();
}

// refreshBoardIfActive — перерисовать доску после PATCH из модалки, если она
// открыта (карточка на доске изменилась: стадия, звёзды, теги, блокеры).
function refreshBoardIfActive() {
  if (state.view === "board") loadBoard();
}

// refreshCurrentView — после удаления записи: доска или список.
function refreshCurrentView() {
  if (state.view === "board") loadBoard();
  else loadList();
}

// --- панель фильтров доски (задача 15.6): строится один раз (setupBoardControls),
// значения подтягиваются из state.board (syncBoardControls) при каждом
// применении маршрута; любое изменение → navigateBoard(). ---

const boardUI = {
  scope: null,
  types: {},
  branch: null,
  workflow: null,
  tag: null,
  parentChip: null,
  parentPicker: null,
  group: null,
};

function setupBoardControls() {
  const box = document.getElementById("board-filters");
  clear(box);

  boardUI.scope = el("select", { class: SELECT_CLASS, attrs: { "aria-label": "scope проекта" } });
  boardUI.scope.addEventListener("change", function () {
    state.board.scope = boardUI.scope.value;
    state.board.parent = "";
    state.board.parentRef = null;
    state.board.doneLimit = 0;
    navigateBoard();
  });

  const typeBox = el("div", { class: "flex items-center gap-x-3" });
  TYPES.forEach(function (t) {
    const id = "board-type-" + t;
    const input = el("input", { attrs: { type: "checkbox", id: id } });
    input.addEventListener("change", function () {
      if (input.checked) state.board.types.add(t);
      else state.board.types.delete(t);
      navigateBoard();
    });
    boardUI.types[t] = input;
    typeBox.appendChild(
      el("div", { class: "flex items-center" }, [
        input,
        el("label", { class: "ml-1 cursor-pointer select-none text-sm", attrs: { for: id }, text: TYPE_LABEL[t] }),
      ])
    );
  });

  function textFilter(placeholder, key) {
    const input = el("input", { class: INPUT_CLASS + " w-36", attrs: { type: "search", placeholder: placeholder, "aria-label": placeholder } });
    function commit() {
      const v = input.value.trim();
      if (v === state.board[key]) return;
      state.board[key] = v;
      navigateBoard();
    }
    input.addEventListener("change", commit);
    input.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter") {
        ev.preventDefault();
        commit();
      }
    });
    return input;
  }
  boardUI.branch = textFilter("ветка", "branch");
  boardUI.workflow = textFilter("workflow", "workflow");
  boardUI.tag = textFilter("тег", "tag");

  const groupId = "board-group-epic";
  boardUI.group = el("input", { attrs: { type: "checkbox", id: groupId } });
  boardUI.group.addEventListener("change", function () {
    state.board.groupByEpic = boardUI.group.checked;
    navigateBoard();
  });

  const resetBtn = el("button", {
    class: BTN_SECONDARY,
    attrs: { type: "button" },
    text: "сбросить фильтры",
    onClick: function () {
      const b = state.board;
      b.types = new Set();
      b.parent = "";
      b.parentRef = null;
      b.branch = "";
      b.workflow = "";
      b.tag = "";
      b.doneLimit = 0;
      b.groupByEpic = false;
      navigateBoard();
    },
  });

  // «только дети эпика/истории»: чип выбранного родителя с крестиком + пикер
  // (тот же renderTaskPicker, только вместо PATCH — фильтр в хэше).
  boardUI.parentChip = el("div", { class: "flex flex-wrap items-center gap-2" });
  boardUI.parentPicker = renderTaskPicker(
    "только дети эпика/истории…",
    [],
    async function (picked) {
      state.board.parent = picked.id;
      state.board.parentRef = picked;
    },
    navigateBoard
  );
  boardUI.parentPicker.classList.add("min-w-64", "max-w-md");

  box.appendChild(
    el("div", { class: "flex flex-wrap items-center gap-x-4 gap-y-2" }, [
      el("label", { class: "flex items-center gap-2 text-sm" }, [el("span", { class: "font-semibold", text: "проект" }), boardUI.scope]),
      typeBox,
      boardUI.branch,
      boardUI.workflow,
      boardUI.tag,
      el("label", { class: "flex items-center gap-1 text-sm", attrs: { for: groupId } }, [boardUI.group, el("span", { text: "группировать по эпику" })]),
      resetBtn,
    ])
  );
  box.appendChild(
    el("div", { class: "mt-2 flex flex-wrap items-start gap-x-4 gap-y-2" }, [
      boardUI.parentChip,
      boardUI.parentPicker,
    ])
  );
}

// populateBoardScopeSelect — опции селекта scope из stats.byScope (все scope с
// записями; задач в них может и не быть — доска тогда пуста).
function populateBoardScopeSelect(stats) {
  const sel = boardUI.scope;
  if (!sel) return;
  clear(sel);
  Object.keys(stats.byScope || {}).sort().forEach(function (scope) {
    sel.appendChild(el("option", { attrs: { value: scope }, text: scope }));
  });
  syncBoardControls();
}

function syncBoardControls() {
  const b = state.board;
  if (!boardUI.scope) return;
  if (b.scope && !Array.from(boardUI.scope.options).some(function (o) { return o.value === b.scope; })) {
    // scope из ссылки, которого нет в статистике — показываем как есть
    boardUI.scope.appendChild(el("option", { attrs: { value: b.scope }, text: b.scope }));
  }
  boardUI.scope.value = b.scope;
  TYPES.forEach(function (t) {
    boardUI.types[t].checked = b.types.has(t);
  });
  boardUI.branch.value = b.branch;
  boardUI.workflow.value = b.workflow;
  boardUI.tag.value = b.tag;
  boardUI.group.checked = b.groupByEpic;

  clear(boardUI.parentChip);
  boardUI.parentPicker.classList.toggle("hidden", !!b.parent);
  if (b.parent) {
    const ref = b.parentRef;
    boardUI.parentChip.appendChild(el("span", { class: "text-sm text-slate-500 dark:text-slate-400", text: "дети:" }));
    boardUI.parentChip.appendChild(
      el("span", { class: "inline-flex items-center gap-2 rounded-full border border-slate-300 px-2 py-0.5 text-sm dark:border-slate-600" }, [
        ref ? taskTypeBadge(ref.type) : null,
        el("span", { text: ref ? (ref.summary || ref.id) : b.parent }),
        el("button", {
          class: "text-slate-400 hover:text-red-600 dark:hover:text-red-400",
          attrs: { type: "button", title: "снять фильтр по родителю", "aria-label": "снять фильтр по родителю" },
          text: "×",
          onClick: function () {
            state.board.parent = "";
            state.board.parentRef = null;
            navigateBoard();
          },
        }),
      ])
    );
    if (!ref) resolveBoardParentRef(b.parent);
  }
}

// resolveBoardParentRef — подпись родителя из ссылки (#/board?parent=<id>):
// в хэше только id, заголовок дочитываем карточкой; ошибка — остаётся id.
async function resolveBoardParentRef(id) {
  try {
    const card = await api("/api/memories/" + encodeURIComponent(id));
    if (state.board.parent !== id) return; // фильтр уже сменили
    const m = card.memory;
    state.board.parentRef = { id: m.id, summary: m.summary, status: m.status, type: m.type };
    syncBoardControls();
  } catch (err) {
    // подпись не критична — чип показывает id
  }
}

// --- загрузка и отрисовка доски ---

function buildBoardQuery() {
  const b = state.board;
  const p = new URLSearchParams();
  p.append("scope", b.scope);
  b.types.forEach(function (t) {
    p.append("type", t);
  });
  if (b.parent) p.set("parent", b.parent);
  if (b.branch) p.set("branch", b.branch);
  if (b.workflow) p.set("workflow", b.workflow);
  if (b.tag) p.set("tag", b.tag);
  if (b.doneLimit > 0) p.set("doneLimit", String(b.doneLimit));
  return p;
}

async function loadBoard() {
  const b = state.board;
  const grid = document.getElementById("board-grid");
  const summary = document.getElementById("board-summary");
  if (b.controller) b.controller.abort();
  const controller = new AbortController();
  b.controller = controller;

  if (!b.scope) {
    clear(grid);
    summary.textContent = "";
    grid.appendChild(emptyState("Нет ни одного scope с записями — доске нечего показывать."));
    b.controller = null;
    return;
  }

  summary.textContent = "загрузка…";
  if (!b.data) {
    clear(grid);
    grid.appendChild(el("div", { class: "text-sm text-slate-500 dark:text-slate-400", text: "загрузка…" }));
  }
  try {
    const data = await api("/api/board?" + buildBoardQuery().toString(), { signal: controller.signal });
    if (b.controller !== controller) return;
    b.data = data;
    renderBoard(data);
  } catch (err) {
    if (err && err.name === "AbortError") return;
    if (b.controller !== controller) return;
    b.data = null;
    summary.textContent = "";
    clear(grid);
    showError(grid, err);
  } finally {
    if (b.controller === controller) b.controller = null;
  }
}

// laneKey/laneLabel — группировка карточек по ближайшему родителю (part_of):
// строка на каждого родителя (эпик или история — доска отдаёт один уровень) и
// строка «без эпика» для карточек без родителя (в том числе самих эпиков).
const NO_LANE = "";

function buildLanes(columns) {
  const lanes = new Map(); // key → { ref, cols: [[cards]…] }
  function lane(key, ref) {
    if (!lanes.has(key)) {
      lanes.set(key, { ref: ref, cols: columns.map(function () { return []; }) });
    }
    return lanes.get(key);
  }
  columns.forEach(function (col, ci) {
    col.cards.forEach(function (card) {
      const key = card.parent ? card.parent.id : NO_LANE;
      lane(key, card.parent).cols[ci].push(card);
    });
  });
  // порядок: строки родителей по подписи, «без эпика» — последней
  const keys = Array.from(lanes.keys()).filter(function (k) { return k !== NO_LANE; });
  keys.sort(function (a, b) {
    const la = (lanes.get(a).ref.summary || a).toLowerCase();
    const lb = (lanes.get(b).ref.summary || b).toLowerCase();
    return la < lb ? -1 : la > lb ? 1 : 0;
  });
  if (lanes.has(NO_LANE)) keys.push(NO_LANE);
  return keys.map(function (k) { return { key: k, ref: lanes.get(k).ref, cols: lanes.get(k).cols }; });
}

function renderBoard(data) {
  const b = state.board;
  const grid = document.getElementById("board-grid");
  const summary = document.getElementById("board-summary");
  const columns = data.columns || [];
  clear(grid);

  let total = 0;
  columns.forEach(function (c) { total += c.total; });
  summary.textContent = "задач: " + total + " · " + b.scope;

  const wrap = el("div", { class: "min-w-[1120px]" });

  // заголовки колонок: метка + счётчик; у done — «показано n из total» и «показать ещё»
  const headRow = el("div", { class: "grid grid-cols-7 gap-2" });
  columns.forEach(function (col) {
    const shown = col.cards.length;
    const truncated = col.total > shown;
    const head = el("div", { class: "flex flex-wrap items-center gap-2 px-1 text-sm font-semibold" }, [
      el("span", { text: STATUS_LABEL[col.status] || col.status }),
      el("span", {
        class: "rounded-full bg-slate-200 px-2 py-0.5 text-xs font-medium text-slate-700 dark:bg-slate-700 dark:text-slate-200",
        text: truncated ? shown + " из " + col.total : String(col.total),
      }),
    ]);
    if (truncated) {
      head.appendChild(
        el("button", {
          class: "text-xs font-normal text-blue-600 underline hover:text-blue-800 dark:text-blue-400",
          attrs: { type: "button" },
          text: "показать ещё (" + (col.total - shown) + ")",
          onClick: function () {
            state.board.doneLimit = Math.max(shown, 1) * 2;
            navigateBoard();
          },
        })
      );
    }
    headRow.appendChild(head);
  });
  wrap.appendChild(headRow);

  if (total === 0) {
    wrap.appendChild(el("div", { class: "mt-2" }, [emptyState("Задач по этим фильтрам нет.")]));
    grid.appendChild(wrap);
    return;
  }

  const lanes = b.groupByEpic
    ? buildLanes(columns)
    : [{ key: NO_LANE, ref: null, cols: columns.map(function (c) { return c.cards; }) }];

  lanes.forEach(function (lane) {
    if (b.groupByEpic) {
      const label = lane.ref ? (lane.ref.summary || lane.ref.id) : "без эпика";
      const header = el("div", { class: "mt-3 flex flex-wrap items-center gap-2 border-t border-slate-200 pt-2 text-sm font-semibold dark:border-slate-700" }, [
        lane.ref ? taskTypeBadge(lane.ref.type) : null,
        lane.ref
          ? el("button", {
              class: "underline hover:text-blue-700 dark:hover:text-blue-400",
              attrs: { type: "button", title: "открыть карточку" },
              text: label,
              onClick: function () { openCard(lane.ref.id); },
            })
          : el("span", { class: "text-slate-500 dark:text-slate-400", text: label }),
      ]);
      wrap.appendChild(header);
    }
    const row = el("div", { class: "mt-2 grid grid-cols-7 gap-2" });
    columns.forEach(function (col, ci) {
      const body = el("div", {
        class: "min-h-24 space-y-2 rounded-md bg-slate-100 p-2 dark:bg-slate-800/60",
        attrs: { "data-status": col.status },
      });
      lane.cols[ci].forEach(function (card) {
        body.appendChild(renderBoardCard(card));
      });
      attachDropTarget(body, col.status);
      row.appendChild(body);
    });
    wrap.appendChild(row);
  });

  grid.appendChild(wrap);
}

// Heroicons «code-bracket» (outline) — иконка чипа ветки.
const BRANCH_PATH = "M17.25 6.75 22.5 12l-5.25 5.25m-10.5 0L1.5 12l5.25-5.25m7.5-3-4.5 16.5";

function renderBoardCard(card) {
  const topRow = el("div", { class: "flex items-center gap-2" }, [
    taskTypeBadge(card.type),
    el("span", { class: "ml-auto text-xs text-amber-500", text: fmtStars(card.importance) }),
  ]);
  const title = el("p", { class: "mt-1 text-sm font-medium text-slate-900 dark:text-slate-100", text: card.summary || card.id });

  const parts = [topRow, title];

  if (card.blocked) {
    const names = (card.blockedBy || []).map(function (b) { return b.summary || b.id; });
    parts.push(
      el("div", {
        class: "mt-1 rounded bg-red-100 px-1.5 py-0.5 text-xs text-red-800 dark:bg-red-900 dark:text-red-200",
        text: "ждёт: " + names.join(", "),
        attrs: { title: "заблокирована: перенос запрещён, пока блокеры не закрыты" },
      })
    );
  }

  if (card.progress) {
    const pct = card.progress.total > 0 ? Math.round((card.progress.done / card.progress.total) * 100) : 0;
    const bar = el("div", { class: "h-1.5 flex-1 overflow-hidden rounded bg-slate-200 dark:bg-slate-700" }, [
      el("div", { class: "h-full bg-emerald-500", attrs: { style: "width:" + pct + "%" } }),
    ]);
    parts.push(
      el("div", { class: "mt-1 flex items-center gap-2", attrs: { title: "готово " + card.progress.done + " из " + card.progress.total } }, [
        bar,
        el("span", { class: "text-xs text-slate-500 dark:text-slate-400", text: card.progress.done + "/" + card.progress.total }),
      ])
    );
  }

  if (card.parent) {
    parts.push(
      el("p", { class: "mt-1 truncate text-xs text-slate-500 dark:text-slate-400", text: "↳ " + (card.parent.summary || card.parent.id), attrs: { title: card.parent.summary || card.parent.id } })
    );
  }

  const chips = el("div", { class: "mt-1 flex flex-wrap gap-1" });
  (card.tags || []).forEach(function (tag) {
    chips.appendChild(el("span", { class: "rounded bg-slate-200 px-1.5 py-0.5 text-xs text-slate-600 dark:bg-slate-700 dark:text-slate-300", text: "#" + tag }));
  });
  if (card.branch) {
    chips.appendChild(
      el("span", { class: "inline-flex items-center gap-1 rounded border border-slate-300 px-1.5 py-0.5 font-mono text-xs text-slate-600 dark:border-slate-600 dark:text-slate-300", attrs: { title: "ветка" } }, [
        icon(BRANCH_PATH, "size-3"),
        el("span", { text: card.branch }),
      ])
    );
  }
  if (card.workflow) {
    chips.appendChild(el("span", { class: "rounded border border-dashed border-slate-300 px-1.5 py-0.5 text-xs text-slate-600 dark:border-slate-600 dark:text-slate-300", text: "⚙ " + card.workflow, attrs: { title: "workflow" } }));
  }
  if (chips.childNodes.length > 0) parts.push(chips);

  const node = el(
    "div",
    {
      class:
        "cursor-grab rounded-md border border-slate-200 bg-white p-2 shadow-sm transition hover:shadow-md active:cursor-grabbing dark:border-slate-700 dark:bg-slate-800",
      attrs: { draggable: "true", "data-id": card.id, "data-status": card.status },
      onClick: function () {
        openCard(card.id);
      },
    },
    parts
  );
  node.addEventListener("dragstart", function (ev) {
    dragging = { id: card.id, status: card.status, node: node, parent: node.parentNode, next: node.nextSibling };
    ev.dataTransfer.effectAllowed = "move";
    ev.dataTransfer.setData("text/plain", card.id);
    node.classList.add("opacity-50");
  });
  node.addEventListener("dragend", function () {
    // dragend приходит сразу после drop — если перенос уже в полёте (moveCard
    // ждёт PATCH), полупрозрачность остаётся до ответа сервера
    if (!node.dataset.inflight) node.classList.remove("opacity-50");
    clearDropHighlights();
    dragging = null;
  });
  return node;
}

// --- drag&drop (нативный HTML5 DnD API, без библиотек): карточка — источник
// (dragstart/dragend выше), тело колонки — цель. Перенос оптимистичный: узел
// сразу переезжает в целевую колонку, затем PATCH {status}; 409 (заблокирована)
// или 400 — узел возвращается на прежнее место, ошибка — тостом с блокерами. ---

let dragging = null;
const DROP_HIGHLIGHT = ["ring-2", "ring-blue-400"];

function clearDropHighlights(except) {
  document.querySelectorAll("#board-grid [data-status]").forEach(function (n) {
    if (n !== except) n.classList.remove.apply(n.classList, DROP_HIGHLIGHT);
  });
}

function attachDropTarget(body, status) {
  body.addEventListener("dragover", function (ev) {
    if (!dragging) return;
    ev.preventDefault();
    ev.dataTransfer.dropEffect = dragging.status === status ? "none" : "move";
    if (dragging.status !== status) {
      body.classList.add.apply(body.classList, DROP_HIGHLIGHT);
      clearDropHighlights(body);
    }
  });
  body.addEventListener("dragleave", function (ev) {
    if (!body.contains(ev.relatedTarget)) body.classList.remove.apply(body.classList, DROP_HIGHLIGHT);
  });
  body.addEventListener("drop", function (ev) {
    ev.preventDefault();
    clearDropHighlights();
    const d = dragging;
    dragging = null;
    if (!d || d.status === status) return;
    moveCard(d, status, body);
  });
}

async function moveCard(d, status, targetBody) {
  const node = d.node;
  // карточка «в полёте» (полупрозрачная) до ответа сервера; dragend, который
  // приходит следом, класс не снимает — см. флаг inflight
  node.dataset.inflight = "1";
  node.classList.add("opacity-50");
  targetBody.appendChild(node); // оптимистично
  try {
    await apiPatch("/api/memories/" + encodeURIComponent(d.id), { status: status });
    // счётчики, прогресс родителя и свимлейны пересчитает сервер
    loadBoard();
  } catch (err) {
    delete node.dataset.inflight;
    node.classList.remove("opacity-50");
    // откат: вернуть узел туда, где он был (если колонка ещё жива)
    if (d.parent && d.parent.isConnected) {
      if (d.next && d.next.parentNode === d.parent) d.parent.insertBefore(node, d.next);
      else d.parent.appendChild(node);
    } else {
      loadBoard();
    }
    showToast(blockedErrorText(err));
  }
}

// --- инициализация ---

function setupFilterControls() {
  const searchInput = document.getElementById("search-input");
  let debounceTimer = null;
  searchInput.addEventListener("input", function () {
    clearTimeout(debounceTimer);
    debounceTimer = setTimeout(function () {
      state.query = searchInput.value;
      loadList();
    }, 400);
  });
  searchInput.addEventListener("keydown", function (ev) {
    if (ev.key === "Enter") {
      clearTimeout(debounceTimer);
      state.query = searchInput.value;
      loadList();
    }
  });

  document.querySelectorAll('input[name="search-mode"]').forEach(function (radio) {
    radio.addEventListener("change", function () {
      if (!radio.checked) return;
      state.mode = radio.value;
      loadList();
    });
  });

  const supersededToggle = document.getElementById("toggle-superseded");
  supersededToggle.addEventListener("change", function () {
    state.showSuperseded = supersededToggle.checked;
    loadList();
  });

  document.getElementById("reset-filters").addEventListener("click", function () {
    state.query = "";
    state.mode = "semantic";
    state.scopes.clear();
    state.kinds.clear();
    state.showSuperseded = false;
    searchInput.value = "";
    document.getElementById("mode-semantic").checked = true;
    supersededToggle.checked = false;
    renderFilterCheckboxes(statsCache);
    loadList();
  });

  document.getElementById("modal-close").addEventListener("click", closeModal);
  document.getElementById("modal-backdrop").addEventListener("click", function (ev) {
    if (ev.target === document.getElementById("modal-backdrop")) closeModal();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeModal();
  });
}

document.addEventListener("DOMContentLoaded", function () {
  setupFilterControls();
  setupInfiniteScroll();
  setupBoardControls();
  loadStats();
  window.addEventListener("hashchange", applyRoute);
  applyRoute();
});
