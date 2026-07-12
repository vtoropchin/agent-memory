// app.js — вся логика web-морды AgentMemory (задача 6.3). Ванильный JS
// (fetch + DOM), без фреймворков. Потребляет JSON API из cmd/memoryd/web.go
// "как есть" (см. DESIGN.md §12, Этап 6). Весь пользовательский текст
// (content/tags/scope/summary/source) вставляется в DOM только через
// textContent — никакого сырого innerHTML с данными из API.

"use strict";

// --- цвета бейджей kind (литеральные классы нужны здесь для Tailwind-сканера
// @source, который ищет полные имена классов текстом в файле) ---
const KIND_BADGE_CLASS = {
  semantic: "bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200",
  episodic: "bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200",
  procedural: "bg-purple-100 text-purple-800 dark:bg-purple-900 dark:text-purple-200",
};
const KIND_BADGE_CLASS_DEFAULT = "bg-slate-100 text-slate-800 dark:bg-slate-700 dark:text-slate-200";

const KIND_LABEL = { semantic: "семантика", episodic: "эпизод", procedural: "процедура" };

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
    throw new Error(msg);
  }
  return body;
}

// apiDelete — мягкое удаление записи (DELETE /api/memories/{id}): запись
// становится архивной (тумстоун), но не стирается безвозвратно.
function apiDelete(path) {
  return api(path, { method: "DELETE" });
}

// --- глобальное состояние фильтров/пагинации ---

const state = {
  query: "",
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

async function loadStats() {
  const box = document.getElementById("dashboard");
  try {
    const stats = await api("/api/stats");
    statsCache = stats;
    renderDashboard(stats);
    renderFilterCheckboxes(stats);
  } catch (err) {
    showError(box, err);
  }
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

  fetchPage(true);
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
      p.delete("offset");
      const data = await api("/api/search?" + p.toString(), { signal: controller.signal });
      if (state.controller !== controller) return; // вытеснено новой загрузкой
      clear(listBox);
      renderSearchResults(data.items || [], summaryBox, listBox);
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

  if (item.supersededBy) {
    topRow.appendChild(
      el("span", {
        class: "rounded-full bg-slate-200 px-2 py-0.5 text-xs text-slate-600 dark:bg-slate-700 dark:text-slate-300",
        text: "архив",
      })
    );
  }
  if (isSearch) {
    topRow.appendChild(
      el("span", {
        class: "ml-auto text-xs text-slate-500 dark:text-slate-400",
        text: "score: " + item.score.toFixed(3) + " (" + item.source + ")",
      })
    );
  }

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
    [topRow, contentEl, tagsRow, el("div", { class: "mt-2" }, [dateEl])]
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
            loadList();
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

function renderCardModal(card) {
  const content = document.getElementById("modal-content");
  clear(content);
  const m = card.memory;

  const kindBadge = el("span", {
    class: "rounded-full px-2 py-0.5 text-xs font-medium " + (KIND_BADGE_CLASS[m.kind] || KIND_BADGE_CLASS_DEFAULT),
    text: KIND_LABEL[m.kind] || m.kind,
  });
  const scopeBadge = el("span", {
    class: "rounded-full border border-slate-300 px-2 py-0.5 text-xs text-slate-600 dark:border-slate-600 dark:text-slate-400",
    text: m.scope,
  });
  const stars = el("span", { class: "text-amber-500 text-sm", text: fmtStars(m.importance) });

  content.appendChild(el("div", { class: "flex flex-wrap items-center gap-2" }, [kindBadge, scopeBadge, stars, vectorStatusBadge(card.embedding)]));

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

  if (m.tags && m.tags.length > 0) {
    const tagsRow = el("div", { class: "mt-3 flex flex-wrap gap-1" });
    m.tags.forEach(function (tag) {
      tagsRow.appendChild(el("span", { class: "rounded bg-slate-100 px-1.5 py-0.5 text-xs text-slate-600 dark:bg-slate-700 dark:text-slate-300", text: "#" + tag }));
    });
    content.appendChild(tagsRow);
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

  const supersededToggle = document.getElementById("toggle-superseded");
  supersededToggle.addEventListener("change", function () {
    state.showSuperseded = supersededToggle.checked;
    loadList();
  });

  document.getElementById("reset-filters").addEventListener("click", function () {
    state.query = "";
    state.scopes.clear();
    state.kinds.clear();
    state.showSuperseded = false;
    searchInput.value = "";
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
  loadStats();
  loadList();
});
