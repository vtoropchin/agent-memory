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

const KINDS = ["semantic", "episodic", "procedural", "debt", "task"];
const KIND_LABEL = { semantic: "факт", episodic: "событие", procedural: "процедура", debt: "долг", task: "задача" };
const KIND_PLURAL = { semantic: "факты", episodic: "события", procedural: "процедуры", debt: "долг", task: "задачи" };
const KIND_HINT = { semantic: "знания и решения", episodic: "события сессий", procedural: "рецепты", debt: "отложено осознанно", task: "доска" };
const STATUSES = ["backlog", "todo", "doing", "review", "testing", "release", "done"];
const STATUS_LABEL = { backlog: "бэклог", todo: "к работе", doing: "в работе", review: "ревью", testing: "проверка", release: "к релизу", done: "готово" };
const TYPE_LABEL = { epic: "эпик", story: "история", task: "задача" };
// подписи связей (§2.3): исходящие «→», входящие «←»
const REL_LABEL = { related: "связано", supersedes: "заменяет", causes: "следствие", part_of: "часть", blocked_by: "заблокирована" };
const REL_IN_LABEL = { related: "связано", supersedes: "заменена", causes: "причина", part_of: "включает", blocked_by: "блокирует" };

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

function statusTag(status) {
  return '<span class="status ' + esc(status) + '">' + esc(STATUS_LABEL[status] || status) + "</span>";
}

// title — заголовок записи для списков: summary, иначе начало content.
function title(m) {
  return m.summary || (m.content || "").slice(0, 120) || m.id;
}

// fmtDate — полная метка даты и времени (для «Служебного»).
function fmtDate(ts) {
  if (!ts) return "—";
  const x = new Date(ts * 1000);
  return x.toLocaleDateString("ru-RU", { day: "2-digit", month: "2-digit", year: "numeric" }) + " " +
    x.toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" });
}

function fmtN(n) { return Number(n || 0).toLocaleString("ru-RU"); }

// imp — важность: 5 сегментов 6×10 (§3). edit — сегменты становятся кнопками
// (→ PATCH {importance} в панели записи, 16.5).
function imp(n, edit) {
  if (!edit) {
    let s = '<span class="imp" role="img" aria-label="важность ' + n + ' из 5">';
    for (let i = 1; i <= 5; i++) s += '<i class="' + (i <= n ? "on" : "") + '"></i>';
    return s + "</span>";
  }
  let s = '<span class="imp edit" role="group" aria-label="Важность">';
  for (let i = 1; i <= 5; i++) {
    s += '<button type="button" class="imp-btn" data-imp="' + i + '" aria-label="важность ' + i + '" aria-pressed="' + (i === n) + '">' +
      '<i class="' + (i <= n ? "on" : "") + '"></i></button>';
  }
  return s + "</span>";
}

function tagChips(tags) {
  return (tags || []).map(function (t) { return '<span class="chip tag">' + esc(t) + "</span>"; }).join("");
}

// scopeShort — «project:memo» → «memo» в тесных местах (строка списка, счётчик фильтров).
function scopeShort(s) { return String(s || "").replace(/^project:/, ""); }

// listRow — строка списков обзора (§2.1): метка 96 px · заголовок-кнопка · дата справа.
function listRow(label, id, text, sub, when) {
  return "<li>" + label + '<span class="t"><button type="button" data-open="' + esc(id) + '">' + esc(text) + "</button>" +
    (sub ? '<span class="sub">' + sub + "</span>" : "") + '</span><span class="when">' + when + "</span></li>";
}

// qs — query-строка из объекта; массивы → повторяемый параметр; пустые опускаются.
function qs(o) {
  const p = new URLSearchParams();
  Object.keys(o).forEach(function (k) {
    const v = o[k];
    if (Array.isArray(v)) v.forEach(function (x) { if (x !== "") p.append(k, x); });
    else if (v !== "" && v != null) p.set(k, v);
  });
  const s = p.toString();
  return s ? "?" + s : "";
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
    err.path = path;
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
    err.path = path;
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

// --- экраны ---

// renderSeq — номер последней перерисовки экрана: ответ устаревшего запроса
// (пользователь успел сменить scope/фильтр) не должен затирать актуальный экран.
let renderSeq = 0;

// screenError — экран целиком заменяется плашкой ошибки API с «Повторить».
function screenError(el, err, retry) {
  el.innerHTML = "";
  el.appendChild(apiErrorNotice(err.path || "/api", err, retry));
}

function placeholder(view, task) {
  const el = $("#screen-" + view + " [data-placeholder]");
  if (el) el.textContent = "Scope: " + scopeLabel(state.scope) + " · экран наполняется задачей " + task + ".";
}

// --- Обзор (§2.1, задача 16.3): только отображение ответов API ---

// emptyScope — пустое состояние (§5): в scope нет живых записей.
function emptyScope() {
  return '<div class="panel"><div class="empty"><h3>В ' + (state.scope ? esc(state.scope) : "памяти") + " пока ничего нет</h3>" +
    "<p>Агент пишет сюда по ходу задач через <code>memory_save</code>. Подключи MCP-сервер memo к проекту — первые записи появятся после первой сессии.</p>" +
    (state.scope ? '<button type="button" class="btn" data-scope="">Показать все scope</button>' : "") + "</div></div>";
}

// embedderNotice — плашка деградации на обзоре (§5) по providers.embedder.available.
function embedderNotice(emb) {
  const name = emb.provider === "noop" || !emb.provider ? "не настроен (noop)" : esc(emb.provider + (emb.model ? "/" + emb.model : "")) + " не отвечает";
  return '<div class="notice warn" style="margin-bottom:var(--s5)"><div class="grow"><strong>Поиск по смыслу недоступен.</strong> Эмбеддер ' + name +
    " — поиск работает только по словам, новые записи копят очередь на эмбеддинг. Сохранение и доска работают как обычно.</div></div>";
}

// providersList — панель «Сервисы»: точка ok/off/na + одна строка пояснения.
function providersList(stats) {
  const p = stats.providers || {}, st = p.store || {}, emb = p.embedder || {}, rs = p.reasoner || {}, jobs = stats.jobs || {};
  const pending = emb.pending ? fmtN(emb.pending) + " " + plural(emb.pending, "запись ждёт", "записи ждут", "записей ждут") + " эмбеддинга" : "очередь на эмбеддинг пуста";
  const embName = emb.provider && emb.provider !== "noop" ? esc(emb.provider) + (emb.model ? ", " + esc(emb.model) : "") + (emb.dim ? " (" + emb.dim + ")" : "") : "noop";
  const embD = emb.available ? "отвечает · " + pending
    : (emb.provider && emb.provider !== "noop" ? "не отвечает" : "не настроен") + " — поиск только по словам, " + pending;
  const rsName = rs.provider && rs.provider !== "noop" ? esc(rs.provider) + (rs.model ? ", " + esc(rs.model) : "") : "noop";
  const rsD = rs.available ? "отвечает · сводки и консолидация эпизодов" : (rs.provider && rs.provider !== "noop" ? "недоступен" : "не настроен") + " — сводки и консолидация эпизодов выключены";
  return '<ul class="providers">' +
    '<li><span class="dot"></span><div>Хранилище — ' + esc(st.driver || "?") + (st.target ? ", " + esc(st.target) : "") +
    '<div class="d">' + fmtN(stats.total + stats.superseded) + " " + plural(stats.total + stats.superseded, "запись", "записи", "записей") + " всего · схема v" + esc(st.schema) + "</div></div></li>" +
    '<li><span class="dot' + (emb.available ? "" : " off") + '"></span><div>Эмбеддер — ' + embName + '<div class="d">' + embD + "</div></div></li>" +
    '<li><span class="dot' + (rs.available ? "" : " off") + '"></span><div>Reasoner — ' + rsName + '<div class="d">' + rsD + "</div></div></li>" +
    '<li><span class="dot na"></span><div>Фоновые джобы<div class="d">decay · dedup · consolidate · reindex — ' +
    (jobs.lastRunAt ? "последний прогон " + esc(rel(jobs.lastRunAt)) : "ещё не запускались") + "</div></div></li></ul>";
}

// boardRows — «На доске»: doing, затем review/testing, затем заблокированные из
// прочих незакрытых колонок (без дублей); порядок и содержимое — из /api/board.
function boardRows(board) {
  const cols = {};
  (board.columns || []).forEach(function (c) { cols[c.status] = c.cards || []; });
  const doing = cols.doing || [], review = (cols.review || []).concat(cols.testing || []);
  const seen = {};
  const out = [];
  doing.concat(review).forEach(function (c) { if (!seen[c.id]) { seen[c.id] = 1; out.push(c); } });
  STATUSES.forEach(function (s) {
    if (s === "done") return;
    (cols[s] || []).forEach(function (c) { if (c.blocked && !seen[c.id]) { seen[c.id] = 1; out.push(c); } });
  });
  const blockedN = STATUSES.reduce(function (n, s) { return s === "done" ? n : n + (cols[s] || []).filter(function (c) { return c.blocked; }).length; }, 0);
  return { rows: out, doing: doing.length, review: review.length, blocked: blockedN };
}

async function renderOverview() {
  const el = $("#screen-overview");
  const scope = state.scope;
  const seq = ++renderSeq;
  el.innerHTML = '<h2 id="h-overview" class="sr-only">Обзор</h2><p class="muted">Загрузка…</p>';
  let stats, recent, board, expiring, byKind, live, archived;
  try {
    const now = Math.floor(Date.now() / 1000);
    const r = await Promise.all([
      api("/api/stats"),
      api("/api/memories" + qs({ scope: scope, limit: 10 })),
      api("/api/board" + qs({ scope: scope })),
      api("/api/memories" + qs({ scope: scope, expiresBefore: now + 14 * 86400, limit: 10 })),
    ]);
    stats = r[0]; recent = r[1]; board = r[2]; expiring = r[3];
    byKind = stats.byKind || {}; live = stats.total; archived = stats.superseded;
    if (scope) {
      // счётчики /api/stats глобальные; для scope берём total из списков с limit=1
      const rs = await Promise.all(KINDS.map(function (k) { return api("/api/memories" + qs({ scope: scope, kind: k, limit: 1 })); })
        .concat([api("/api/memories" + qs({ scope: scope, superseded: 1, limit: 1 }))]));
      byKind = {};
      KINDS.forEach(function (k, i) { byKind[k] = rs[i].total; });
      live = (stats.byScope || {})[scope] || 0;
      archived = Math.max(0, rs[5].total - live);
    }
  } catch (err) {
    if (seq !== renderSeq) return;
    screenError(el, err, renderOverview);
    return;
  }
  if (seq !== renderSeq) return;
  state.stats = stats;
  renderScopeSelect();

  const now = Math.floor(Date.now() / 1000);
  const recentItems = (recent.items || []).filter(function (m) { return !m.supersededBy && m.createdAt >= now - 7 * 86400; });
  const b = boardRows(board);
  let updated = 0;
  (recent.items || []).concat(b.rows).forEach(function (m) { if (m.updatedAt > updated) updated = m.updatedAt; });
  const emb = (stats.providers || {}).embedder || {};

  let html = '<h2 id="h-overview" class="sr-only">Обзор</h2>';
  if (!live) { el.innerHTML = html + emptyScope(); return; }
  html += '<div class="headline"><span class="num">' + fmtN(live) + "</span><span>" + plural(live, "запись", "записи", "записей") + " в " + esc(scopeLabel(scope)) + "</span>" +
    '<span class="muted">' + fmtN(archived) + " в архиве · обновлено " + esc(rel(updated)) + "</span></div>";
  if (!emb.available) html += embedderNotice(emb);
  html += '<div class="overview"><div class="stack">' +
    '<section class="kinds" aria-label="Записи по видам">' + KINDS.map(function (k) {
      return '<button type="button" class="rule-' + k + '" data-goto-kind="' + k + '"><span class="kind ' + k + '">' + KIND_PLURAL[k] + '</span><span class="n">' + fmtN(byKind[k] || 0) + '</span><span class="d">' + KIND_HINT[k] + "</span></button>";
    }).join("") + "</section>" +
    '<section class="panel" aria-labelledby="h-recent"><header><h2 id="h-recent">Добавлено за 7 дней</h2><span class="muted">' + recentItems.length + '</span><span class="grow"></span><button type="button" class="btn quiet sm" data-goto="memory">Вся память</button></header><div class="body">' +
    (recentItems.length ? '<ul class="rows">' + recentItems.map(function (m) { return listRow(kindTag(m.kind), m.id, title(m), "", esc(rel(m.createdAt))); }).join("") + "</ul>"
      : '<p class="muted">За неделю ничего не добавилось. Агент пишет в память по ходу задач — записи появятся здесь.</p>') + "</div></section>" +
    '<section class="panel" aria-labelledby="h-doing"><header><h2 id="h-doing">На доске</h2><span class="muted">' + b.doing + " в работе · " + b.review + " на ревью и проверке · " + b.blocked + ' заблокировано</span><span class="grow"></span><button type="button" class="btn quiet sm" data-goto="board">Доска</button></header><div class="body">' +
    (b.rows.length ? '<ul class="rows">' + b.rows.map(function (c) {
      const blk = c.blocked && c.blockedBy && c.blockedBy.length ? '<span class="blk">заблокирована</span> · ждёт: ' + esc(c.blockedBy[0].summary || c.blockedBy[0].id) : "";
      return listRow(statusTag(c.status), c.id, c.summary || c.id, blk, (c.branch ? "<code>" + esc(c.branch) + "</code> · " : "") + esc(rel(c.updatedAt)));
    }).join("") + "</ul>" : '<p class="muted">Ничего не в работе и не на проверке.</p>') + "</div></section></div>" +
    '<div class="stack"><section class="panel" aria-labelledby="h-prov"><header><h2 id="h-prov">Сервисы</h2></header><div class="body">' + providersList(stats) + "</div></section>";
  const exp = (expiring.items || []).filter(function (m) { return !m.supersededBy; });
  if (exp.length) {
    html += '<section class="panel" aria-labelledby="h-exp"><header><h2 id="h-exp">Истекает скоро</h2></header><div class="body"><ul class="rows">' +
      exp.map(function (m) { return listRow(kindTag(m.kind), m.id, title(m), "", esc(rel(m.expiresAt))); }).join("") + "</ul></div></section>";
  }
  const scopes = Object.keys(stats.byScope || {}).sort(function (a, b2) { return stats.byScope[b2] - stats.byScope[a] || a.localeCompare(b2); });
  html += '<section class="panel" aria-labelledby="h-scopes"><header><h2 id="h-scopes">Scope</h2><span class="muted">живые записи</span></header><div class="body"><ul class="scopes-list">' +
    scopes.map(function (s) {
      return '<li><button type="button" data-scope="' + esc(s) + '"' + (s === scope ? ' aria-current="true"' : "") + ">" + esc(s) + (s === stats.homeScope ? ' <span class="muted">домашний</span>' : "") + '</button><span class="n">' + fmtN(stats.byScope[s]) + "</span></li>";
    }).join("") + "</ul></div></section></div></div>";
  el.innerHTML = html;
}

const screens = {
  overview: { render: renderOverview },
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
  const gotoKind = t.closest("[data-goto-kind]");
  if (gotoKind) { navigate({ view: "memory", params: new URLSearchParams({ kind: gotoKind.getAttribute("data-goto-kind") }), id: "" }); return; }
  const goto = t.closest("[data-goto]");
  if (goto) { navigate({ view: goto.getAttribute("data-goto"), params: new URLSearchParams(), id: "" }); return; }
  const sc = t.closest("[data-scope]");
  if (sc) { navigate({ scope: sc.getAttribute("data-scope") }); return; }
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
