// app.js — каркас новой веб-морды memo web (Этап 16, задача 16.2).
//
// Спецификация — design/memo-web-redesign.md, макет — design/memo-web-prototype.html.
// Фронт — адаптер поверх JSON API (cmd/memo/web.go): доменной логики здесь нет,
// рисуется ответ сервера, ошибки маппятся в тексты §5. Vanilla JS, без сборки и
// сторонних библиотек (решение Этапа 6); стили — cmd/memo/tailwind.input.css.
//
// Устройство:
//   - состояние живёт в URL-хэше: раздел (#/, #/memory, #/board), scope, фильтры
//     экрана и id открытой записи; parseRoute разбирает хэш в state, navigate пишет
//     новый хэш, hashchange → applyRoute — единственная точка перерисовки (как в
//     15.6: ссылка воспроизводит экран);
//   - экраны — объекты screens[view] с render(); в этой задаче они заглушки,
//     наполняются задачами 16.3 (обзор), 16.4 (память), 16.6 (доска);
//   - панель записи (drawer) открывается по id= в хэше, тело — задача 16.5;
//   - showToast, api, rel — общие хелперы для всех экранов.

"use strict";

// --- утилиты ---

const $ = function (sel, root) { return (root || document).querySelector(sel); };
const $$ = function (sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); };

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
  });
}

const KIND_LABEL = { semantic: "факт", episodic: "событие", procedural: "процедура", debt: "долг", task: "задача" };

// plural — форма слова по числу: plural(3, "день", "дня", "дней") → "дня".
function plural(n, one, few, many) {
  const m = n % 10, M = n % 100;
  if (M >= 11 && M <= 19) return many;
  if (m === 1) return one;
  if (m >= 2 && m <= 4) return few;
  return many;
}

// rel — относительная дата (§3): «2 ч назад», «через 3 дня»; старше 30 дней —
// дата без времени; полные метки — в «Служебном» (16.5). ts — unix-секунды.
function rel(ts) {
  if (!ts) return "—";
  const s = Math.floor(Date.now() / 1000) - ts;
  if (s < 0) {
    const dd = Math.max(1, Math.round(-s / 86400));
    return "через " + dd + " " + plural(dd, "день", "дня", "дней");
  }
  if (s < 3600) return Math.max(1, Math.round(s / 60)) + " мин назад";
  if (s < 86400) return Math.round(s / 3600) + " ч назад";
  const dd = Math.round(s / 86400);
  if (dd < 30) return dd + " " + plural(dd, "день", "дня", "дней") + " назад";
  return new Date(ts * 1000).toLocaleDateString("ru-RU", { day: "2-digit", month: "2-digit", year: "numeric" });
}

// scopeLabel — подпись scope для заголовков: "" → «все scope».
function scopeLabel(scope) {
  return scope ? scope : "все scope";
}

// kindTag — «вид» записи: цветной квадратик + слово (не заливка чипа, §3).
function kindTag(kind) {
  return '<span class="kind ' + esc(kind) + '">' + esc(KIND_LABEL[kind] || kind) + "</span>";
}

// --- API ---

// api — fetch к JSON API. Не-2xx → Error с полями status и body (тело ответа,
// напр. 409 {error, blockers} при переносе заблокированной задачи) — экраны
// ветвятся по коду и собирают тексты §5; ошибка сети → status 0.
async function api(path, opts) {
  let resp;
  try {
    resp = await fetch(path, opts);
  } catch (e) {
    const err = new Error("ошибка соединения");
    err.status = 0;
    err.body = null;
    throw err;
  }
  let body = null;
  try {
    body = await resp.json();
  } catch (e) {
    // не JSON — body = null, сообщение соберём из статуса
  }
  if (!resp.ok) {
    const err = new Error(body && body.error ? body.error : "HTTP " + resp.status);
    err.status = resp.status;
    err.body = body;
    throw err;
  }
  return body;
}

// apiErrorNotice — плашка ошибки API с кнопкой «Повторить»: сеть (status 0) —
// текст «Сервер памяти не отвечает» (§5); HTTP-ошибка — сообщение сервера.
// Без role=alert: aria-live только на тостах и счётчике результатов (§4).
function apiErrorNotice(path, err, retry) {
  const box = document.createElement("div");
  box.className = "notice danger";
  box.innerHTML = '<div class="grow">' + (err.status === 0
    ? "<strong>Сервер памяти не отвечает.</strong> Запрос к <code>" + esc(path) +
      "</code> завершился ошибкой соединения. Проверь, что <code>memo web</code> запущен на " + esc(location.host) + "."
    : "<strong>Ошибка сервера памяти.</strong> Запрос к <code>" + esc(path) + "</code>: " + esc(err.message) + ".") +
    "</div>";
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "btn";
  btn.textContent = "Повторить";
  btn.addEventListener("click", retry);
  box.appendChild(btn);
  return box;
}

// --- состояние и роутер ---

const state = {
  view: "overview",
  scope: "",           // глобальный фильтр scope; "" — все scope
  params: new URLSearchParams(), // фильтры текущего экрана (без scope и id)
  id: "",              // id открытой записи (панель)
  stats: null,         // ответ /api/stats: scope-селект, homeScope
};

let lastFocus = null;   // элемент, открывший панель; к нему возвращается фокус

// parseRoute — хэш → {view, scope, params, id}. "#/" — обзор, "#/memory",
// "#/board"; неизвестный путь (в т.ч. #main от skip link) — обзор.
function parseRoute() {
  const hash = location.hash || "#/";
  const path = hash.replace(/^#/, "");
  const qIdx = path.indexOf("?");
  const route = qIdx === -1 ? path : path.slice(0, qIdx);
  const params = new URLSearchParams(qIdx === -1 ? "" : path.slice(qIdx + 1));
  const view = route === "/memory" ? "memory" : route === "/board" ? "board" : "overview";
  const scope = params.get("scope") || "";
  const id = (params.get("id") || "").trim();
  params.delete("scope");
  params.delete("id");
  return { view: view, scope: scope, params: params, id: id };
}

// buildHash — обратная операция: {view, scope, params, id} → "#/…?…". scope —
// первым параметром, id — последним; пустые опускаются, чтобы одинаковое
// состояние давало одинаковую строку.
function buildHash(r) {
  const p = new URLSearchParams();
  if (r.scope) p.set("scope", r.scope);
  r.params.forEach(function (v, k) { if (v !== "") p.append(k, v); });
  if (r.id) p.set("id", r.id);
  const q = p.toString();
  return "#" + (r.view === "overview" ? "/" : "/" + r.view) + (q ? "?" + q : "");
}

// navigate — применить изменение состояния: записать хэш (hashchange сделает
// перерисовку); если хэш уже такой — перерисовать напрямую. patch: {view, scope,
// params, id}, отсутствующие поля берутся из state; смена раздела сбрасывает
// фильтры экрана (params), scope переживает переключение. replace — переписать
// хэш без записи в историю (подстановка домашнего scope на старте).
function navigate(patch, replace) {
  const next = {
    view: patch.view != null ? patch.view : state.view,
    scope: patch.scope != null ? patch.scope : state.scope,
    params: patch.params != null ? patch.params : (patch.view != null && patch.view !== state.view ? new URLSearchParams() : state.params),
    id: patch.id != null ? patch.id : state.id,
  };
  // элемент, открывший панель, запоминаем при открытии из закрытого состояния — к
  // нему вернётся фокус; переход по связи внутри панели (16.5) его не перезаписывает
  if (next.id && !state.id) lastFocus = document.activeElement;
  const h = buildHash(next);
  if (location.hash === h || (h === "#/" && !location.hash)) { applyRoute(); return; }
  if (replace) location.replace(h);
  else location.hash = h;
}

// applyRoute — единственная точка перерисовки: хэш → state → шапка, экран, панель.
function applyRoute() {
  const r = parseRoute();
  state.view = r.view;
  state.scope = r.scope;
  state.params = r.params;
  state.id = r.id;
  renderNav();
  renderScopeSelect();
  renderView();
  renderDrawer();
}

// --- шапка: разделы и селект scope ---

function renderNav() {
  $$(".nav a[data-view]").forEach(function (a) {
    const view = a.getAttribute("data-view");
    // ссылка раздела несёт текущий scope: он переживает переключение раздела
    a.setAttribute("href", buildHash({ view: view, scope: state.scope, params: new URLSearchParams(), id: "" }));
    if (view === state.view) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
}

// renderScopeSelect — «все scope» + stats.byScope (по алфавиту), домашний помечен
// по homeScope; выбранный — state.scope (даже если его нет в статистике: ссылка
// на пустой scope не должна молча переключать фильтр).
function renderScopeSelect() {
  const sel = $("#scope-global");
  const stats = state.stats;
  const scopes = stats && stats.byScope ? Object.keys(stats.byScope).sort() : [];
  if (state.scope && scopes.indexOf(state.scope) === -1) scopes.push(state.scope);
  const home = stats && stats.homeScope ? stats.homeScope : "";
  sel.innerHTML = '<option value="">все scope</option>' + scopes.map(function (s) {
    return '<option value="' + esc(s) + '">' + esc(s) + (s === home ? " (домашний)" : "") + "</option>";
  }).join("");
  sel.value = state.scope;
}

// defaultScope — scope при пустом стартовом хэше: домашний scope инстанса
// (stats.homeScope), если в нём есть записи; иначе "" — все scope.
function defaultScope(stats) {
  if (stats && stats.homeScope && stats.byScope && stats.byScope[stats.homeScope]) return stats.homeScope;
  return "";
}

// --- экраны (заглушки; наполнение — 16.3/16.4/16.6) ---

function placeholder(view, task) {
  const el = $("#screen-" + view + " [data-placeholder]");
  if (el) el.textContent = "Scope: " + scopeLabel(state.scope) + " · экран наполняется задачей " + task + ".";
}

const screens = {
  overview: { render: function () { placeholder("overview", "16.3"); } },
  memory: { render: function () { placeholder("memory", "16.4"); } },
  board: { render: function () { placeholder("board", "16.6"); } },
};

function renderView() {
  Object.keys(screens).forEach(function (v) { $("#screen-" + v).hidden = v !== state.view; });
  // доске нужна ширина под 7 колонок; остальные экраны — не шире 1400 px
  $("#main").classList.toggle("wide", state.view === "board");
  screens[state.view].render();
}

// --- панель записи (drawer, §2.3) ---

let drawerId = "";      // id записи, которая сейчас отрисована в панели

function openRecord(id) { navigate({ id: id }); }
function closeRecord() { navigate({ id: "" }); }

// setBackgroundInert — всё, кроме панели и тостов, недоступно для фокуса и
// скринридера, пока панель открыта (§4).
function setBackgroundInert(on) {
  Array.prototype.forEach.call(document.body.children, function (el) {
    if (el.id === "drawer-root" || el.id === "toast-root" || el.tagName === "SCRIPT") return;
    el.inert = on;
  });
}

function renderDrawer() {
  const root = $("#drawer-root");
  if (!state.id) {
    if (!drawerId && !root.firstChild) return;
    drawerId = "";
    root.innerHTML = "";
    setBackgroundInert(false);
    // возврат фокуса: на открывший элемент, если он ещё в документе, иначе на main
    const back = lastFocus && document.contains(lastFocus) && lastFocus !== document.body ? lastFocus : $("#main");
    lastFocus = null;
    back.focus();
    return;
  }
  if (state.id === drawerId) return;
  drawerId = state.id;
  setBackgroundInert(true);
  root.innerHTML =
    '<div class="drawer-backdrop" data-close></div>' +
    '<aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="rec-title" tabindex="-1">' +
    '<header><span id="rec-kind"></span><div class="grow"><span class="muted" id="rec-scope"></span></div>' +
    '<button type="button" class="btn quiet sm" data-close aria-label="Закрыть панель">Закрыть ✕</button></header>' +
    '<div class="content"><h2 id="rec-title" class="summary">Запись</h2><div id="rec-body"></div></div>' +
    "</aside>";
  $(".drawer", root).focus();
  loadRecord(state.id);
}

// loadRecord — GET /api/memories/{id} (карточка: {memory, neighbors, embedding,
// semanticNeighbors, parent, children, …}) → шапка (вид, scope) и заглушка тела
// (summary + текст серифом); полное тело панели — задача 16.5.
async function loadRecord(id) {
  const body = $("#rec-body");
  if (!body) return;
  body.innerHTML = '<p class="muted">Загрузка…</p>';
  let m;
  try {
    m = (await api("/api/memories/" + encodeURIComponent(id))).memory;
  } catch (err) {
    if (drawerId !== id || !$("#rec-body")) return;
    body.innerHTML = "";
    if (err.status === 404) {
      body.innerHTML = '<div class="notice warn">Записи <code>' + esc(id) + "</code> нет — удалена или id указан с ошибкой.</div>";
    } else {
      body.appendChild(apiErrorNotice("/api/memories/" + id, err, function () { loadRecord(id); }));
    }
    return;
  }
  if (drawerId !== id || !$("#rec-body")) return;
  $("#rec-kind").innerHTML = kindTag(m.kind);
  $("#rec-scope").textContent = m.scope;
  $("#rec-title").textContent = m.summary || m.content.slice(0, 120);
  body.innerHTML =
    (m.summary ? '<div class="prose">' + esc(m.content) + "</div>" : "") +
    '<p class="muted" style="margin-top:var(--s4)">создана ' + esc(rel(m.createdAt)) +
    " · просмотр, связи и правка — задача 16.5.</p>";
}

// --- тосты (§5): один активный, замещается следующим, гаснет через 2,6 с ---

let toastTimer = null;
function showToast(text) {
  const root = $("#toast-root");
  root.innerHTML = '<div class="toast">' + esc(text) + "</div>";
  clearTimeout(toastTimer);
  toastTimer = setTimeout(function () { root.innerHTML = ""; }, 2600);
}

// --- события ---

document.addEventListener("click", function (e) {
  const t = e.target;
  if (!(t instanceof Element)) return;
  if (t.closest("[data-close]")) { closeRecord(); return; }
  const open = t.closest("[data-open]");
  if (open) { openRecord(open.getAttribute("data-open")); return; }
  if (t.closest(".skip")) {
    // skip link не должен менять хэш (он — маршрут): просто переводим фокус в main
    e.preventDefault();
    $("#main").focus();
  }
});

document.addEventListener("keydown", function (e) {
  if (e.key === "Escape" && state.id) { e.preventDefault(); closeRecord(); }
});

$("#scope-global").addEventListener("change", function (e) {
  navigate({ scope: e.target.value });
});

window.addEventListener("hashchange", applyRoute);

// --- старт: статистика для селекта scope, домашний scope при пустом хэше, маршрут ---

async function loadStats() {
  const notice = $("#notice-root");
  notice.innerHTML = "";
  try {
    state.stats = await api("/api/stats");
  } catch (err) {
    state.stats = null;
    notice.appendChild(apiErrorNotice("/api/stats", err, init));
    return false;
  }
  return true;
}

async function init() {
  $("#brand-host").textContent = "память агента · " + location.host;
  const ok = await loadStats();
  const bare = !location.hash || location.hash === "#/" || location.hash === "#";
  if (ok && bare) {
    // пустой стартовый хэш — открываем домашний scope (как доска в 15.6), не
    // записывая шаг в историю; hashchange дорисует
    const def = defaultScope(state.stats);
    if (def) { navigate({ view: "overview", scope: def, params: new URLSearchParams(), id: "" }, true); return; }
  }
  applyRoute();
}

init();
