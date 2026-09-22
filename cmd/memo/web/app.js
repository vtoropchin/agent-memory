// app.js — новая веб-морда memo web (Этап 16: каркас 16.2, экраны 16.3–16.6).
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
//   - экраны — объекты screens[view] с render(): обзор (16.3), память (16.4),
//     доска (16.6); ответ устаревшего запроса отбрасывается по renderSeq;
//   - панель записи (drawer) открывается по id= в хэше (16.5), для задачи —
//     поля доски, иерархия и блокировки (16.6); правки — PATCH/DELETE через
//     patchRecord/forgetRecord: успех → перечитать карточку и экран + тост,
//     ошибка → тост с текстом сервера;
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

// --- Память (§2.2, задача 16.4): поиск, фильтры, список; ранжирование — бэкенд ---

const PAGE = 50;
let focusSearch = false;   // после перерисовки вернуть фокус в поле поиска (ввод, «/», смена режима)

// memoryParams — фильтры экрана из хэша: q, mode (semantic|lexical), kind[], archived, offset.
function memoryParams() {
  const p = state.params;
  const mode = p.get("mode") === "lexical" ? "lexical" : "semantic";
  const kinds = p.getAll("kind").filter(function (k) { return KINDS.indexOf(k) !== -1; });
  return { q: (p.get("q") || "").trim(), mode: mode, kinds: kinds, archived: p.get("archived") === "1", offset: Math.max(0, parseInt(p.get("offset") || "0", 10) || 0) };
}

// setMemoryParams — записать фильтры в хэш (смена фильтра сбрасывает offset).
function setMemoryParams(patch) {
  const cur = memoryParams();
  const next = Object.assign({}, cur, patch);
  const p = new URLSearchParams();
  if (next.q) p.set("q", next.q);
  if (next.mode === "lexical") p.set("mode", "lexical");
  next.kinds.forEach(function (k) { p.append("kind", k); });
  if (next.archived) p.set("archived", "1");
  if (patch.offset != null && next.offset > 0) p.set("offset", String(next.offset));
  navigate({ params: p });
}

// highlight — подсветка слов запроса в тексте; работает по уже экранированной
// строке и экранированному слову, чтобы сырые данные не попали в innerHTML.
function highlight(text, q) {
  let out = esc(text);
  if (!q) return out;
  q.split(/\s+/).filter(function (w) { return w.length > 2; }).forEach(function (w) {
    const re = new RegExp("(" + esc(w).replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + ")", "gi");
    out = out.replace(re, '<mark class="hl">$1</mark>');
  });
  return out;
}

function memoryRow(m, q) {
  const arch = !!m.supersededBy;
  const src = m.source === "lexical+vector" ? "слова + смысл" : m.source === "vector" ? "по смыслу" : m.source === "lexical" ? "по словам" : "";
  return '<article class="row rule-' + esc(m.kind) + '" role="listitem" data-open="' + esc(m.id) + '" aria-selected="' + (state.id === m.id) + '">' +
    '<div class="meta">' + kindTag(m.kind) + '<span class="scope">' + esc(scopeShort(m.scope)) + "</span>" + (m.kind === "task" && m.status ? statusTag(m.status) : "") + "</div>" +
    '<div class="main"><button type="button" class="title' + (arch ? " archived" : "") + '" data-open="' + esc(m.id) + '">' + highlight(title(m), q) + "</button>" +
    '<div class="tags">' + tagChips(m.tags) + (arch ? '<span class="chip">в архиве · заменена</span>' : "") +
    (m.expiresAt ? '<span class="chip">истекает ' + esc(rel(m.expiresAt)) + "</span>" : "") + "</div></div>" +
    '<div class="side">' + imp(m.importance) + "<span>" + esc(rel(m.createdAt)) + "</span>" + (src ? '<span class="src">' + src + "</span>" : "") + "</div></article>";
}

async function renderMemory() {
  const el = $("#screen-memory");
  const f = memoryParams();
  const scope = state.scope;
  const seq = ++renderSeq;
  const active = f.kinds.length + (f.archived ? 1 : 0) + (scope ? 1 : 0) + (f.q ? 1 : 0);
  // шапка экрана рисуется сразу — поле поиска доступно, пока грузится список
  el.innerHTML = '<h2 id="h-memory" class="sr-only">Память</h2>' +
    '<div class="toolbar"><div class="search"><svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="7" cy="7" r="5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M11 11l3.5 3.5" stroke="currentColor" stroke-width="1.6"/></svg>' +
    '<input type="search" id="q" value="' + esc(f.q) + '" name="q" autocomplete="off" spellcheck="false" placeholder="Что искать: слово, термин или смысл… (/)" aria-label="Поиск по памяти"></div>' +
    '<div class="seg" role="radiogroup" aria-label="Режим поиска">' +
    '<button type="button" role="radio" aria-checked="' + (f.mode === "semantic") + '" data-mode="semantic">по смыслу</button>' +
    '<button type="button" role="radio" aria-checked="' + (f.mode === "lexical") + '" data-mode="lexical">по словам</button></div></div>' +
    '<div class="filters"><span>Вид</span><div class="checks" role="group" aria-label="Вид записи">' + KINDS.map(function (k) {
      return '<label><input type="checkbox" data-kind="' + k + '"' + (f.kinds.indexOf(k) !== -1 ? " checked" : "") + '><span class="kind ' + k + '">' + KIND_PLURAL[k] + "</span></label>";
    }).join("") + "</div>" +
    '<label class="archived"><input type="checkbox" id="archived"' + (f.archived ? " checked" : "") + "> с архивом</label>" +
    '<div class="active">' + (active ? "<span>" + active + " " + plural(active, "фильтр", "фильтра", "фильтров") + (scope ? " · " + esc(scopeShort(scope)) : "") + '</span><button type="button" class="btn quiet sm" data-reset>Сбросить всё</button>' : "<span>фильтров нет</span>") + "</div></div>" +
    '<div id="mem-results"><p class="muted">Загрузка…</p></div>';
  if (focusSearch) {
    focusSearch = false;
    const i = $("#q");
    i.focus();
    i.setSelectionRange(i.value.length, i.value.length);
  }
  const box = $("#mem-results");
  let items, total, vector = true;
  try {
    if (f.q) {
      const r = await api("/api/search" + qs({ q: f.q, mode: f.mode, kind: f.kinds, scope: scope, limit: PAGE }));
      items = r.items || []; total = items.length; vector = r.vector !== false;
    } else {
      const r = await api("/api/memories" + qs({ scope: scope, kind: f.kinds, superseded: f.archived ? 1 : "", limit: PAGE, offset: f.offset }));
      items = r.items || []; total = r.total;
    }
  } catch (err) {
    if (seq !== renderSeq) return;
    screenError(box, err, renderMemory);
    return;
  }
  if (seq !== renderSeq) return;
  const degraded = f.q && f.mode === "semantic" && !vector;
  let html = "";
  if (degraded) {
    html += '<div class="notice warn" style="margin-bottom:var(--s3)"><div class="grow"><strong>Поиск только по словам.</strong> Эмбеддер не отвечает, поэтому запрос не эмбеддится и перефразировки не найдутся — ищи точными терминами. Как только эмбеддер вернётся, режим «по смыслу» включится сам.</div></div>';
  }
  html += '<div class="results-head" aria-live="polite"><span>' + (f.q
    ? items.length + " " + plural(items.length, "результат", "результата", "результатов") + " по «" + esc(f.q) + "»"
    : (total ? (f.offset + 1) + "–" + (f.offset + items.length) + " из " + fmtN(total) : "0") + " — новые сверху") + "</span>" +
    (f.q && f.mode === "semantic" && vector ? "<span>лексика + вектор, RRF</span>" : "") + "</div>";
  if (items.length) {
    html += '<div class="list" role="list" aria-label="Записи">' + items.map(function (m) { return memoryRow(m, f.q); }).join("") + "</div>";
    if (!f.q && total > PAGE) {
      html += '<div class="pager">' + (f.offset > 0 ? '<button type="button" class="btn sm" data-page="' + Math.max(0, f.offset - PAGE) + '">← Назад</button>' : "") +
        (f.offset + PAGE < total ? '<button type="button" class="btn sm" data-page="' + (f.offset + PAGE) + '">Дальше →</button>' : "") + "</div>";
    }
  } else if (f.q) {
    html += '<div class="list"><div class="empty"><h3>По запросу «' + esc(f.q) + "» ничего не нашлось</h3><p>" +
      (degraded ? "Поиск сейчас только по словам: попробуй точный термин из записи или сними фильтры." : "Проверь фильтры вида и scope — они сужают выдачу. Или переформулируй: поиск по смыслу находит и перефразировки.") +
      '</p><button type="button" class="btn" data-reset>Сбросить фильтры</button></div></div>';
  } else {
    html += '<div class="list"><div class="empty"><h3>Записей нет</h3><p>' +
      (f.kinds.length ? "Под выбранные виды записей в " + esc(scopeLabel(scope)) + " ничего не попадает." : "В " + esc(scopeLabel(scope)) + " пока ничего нет — агент пишет сюда по ходу задач через <code>memory_save</code>.") +
      "</p>" + (f.kinds.length || scope || f.archived ? '<button type="button" class="btn" data-reset>Сбросить фильтры</button>' : "") + "</div></div>";
  }
  box.innerHTML = html;
}

let qTimer = null;
document.addEventListener("input", function (e) {
  if (e.target.id !== "q") return;
  clearTimeout(qTimer);
  const v = e.target.value;
  qTimer = setTimeout(function () { focusSearch = true; setMemoryParams({ q: v.trim() }); }, 260);
});

document.addEventListener("change", function (e) {
  const t = e.target;
  if (!(t instanceof Element)) return;
  if (t.hasAttribute("data-kind")) {
    const f = memoryParams();
    const k = t.getAttribute("data-kind");
    const kinds = f.kinds.filter(function (x) { return x !== k; });
    if (t.checked) kinds.push(k);
    setMemoryParams({ kinds: kinds });
    return;
  }
  if (t.id === "archived") { setMemoryParams({ archived: t.checked }); return; }
});

// --- Доска (§2.4, задача 16.6): рисует ответ board.Build; правило блокировки — бэкенд ---

const DONE_LIMIT = 30;

// boardParams — фильтры доски из хэша: type, parent, tag, group (по умолчанию
// включены дорожки; «0» — выключены), doneLimit.
function boardParams() {
  const p = state.params;
  return {
    type: p.get("type") || "", parent: p.get("parent") || "", tag: p.get("tag") || "",
    lanes: p.get("group") !== "0", doneLimit: Math.max(0, parseInt(p.get("doneLimit") || "0", 10) || 0),
  };
}

function setBoardParams(patch) {
  const next = Object.assign({}, boardParams(), patch);
  const p = new URLSearchParams();
  if (next.type) p.set("type", next.type);
  if (next.parent) p.set("parent", next.parent);
  if (next.tag) p.set("tag", next.tag);
  if (!next.lanes) p.set("group", "0");
  if (next.doneLimit) p.set("doneLimit", String(next.doneLimit));
  navigate({ params: p });
}

function boardCard(c, lanes) {
  const pr = c.progress;
  return '<article class="card ' + esc(c.type) + (c.blocked ? " blocked" : "") + '" draggable="true" data-id="' + esc(c.id) + '" data-status="' + esc(c.status) + '" aria-label="' + esc(c.summary || c.id) + '">' +
    '<div class="top"><span>' + esc(TYPE_LABEL[c.type] || c.type) + "</span>" +
    (c.blocked ? '<span class="blk" title="' + esc((c.blockedBy || []).map(function (b) { return b.summary || b.id; }).join("; ")) + '">заблокирована</span>' : "") +
    '<span class="grow"></span>' + imp(c.importance) + "</div>" +
    '<button type="button" class="ttl" data-open="' + esc(c.id) + '">' + esc(c.summary || c.id) + "</button>" +
    (pr && pr.total ? '<div class="prog"><div class="progress"><i style="width:' + Math.round(pr.done / pr.total * 100) + '%"></i></div>' + pr.done + "/" + pr.total + "</div>" : "") +
    '<div class="foot">' + (c.branch ? "<code>" + esc(c.branch) + "</code>" : "") +
    (!lanes && c.parent ? "<span>↳ " + esc((c.parent.summary || c.parent.id).slice(0, 28)) + "</span>" : "") +
    "<span>" + esc(rel(c.updatedAt)) + "</span>" +
    '<span class="move"><label class="sr-only" for="mv-' + esc(c.id) + '">Переместить в стадию</label><select id="mv-' + esc(c.id) + '" data-move="' + esc(c.id) + '">' +
    STATUSES.map(function (s) { return '<option value="' + s + '"' + (s === c.status ? " selected" : "") + ">" + STATUS_LABEL[s] + "</option>"; }).join("") +
    "</select></span></div></article>";
}

function boardColumn(col, lanes, doneLimit) {
  const s = col.status, cards = col.cards || [];
  // свимлейны — группировка по parent из ответа (представление, не правило домена)
  const groups = {}, labels = {};
  cards.forEach(function (c) {
    const key = lanes && c.parent ? c.parent.id : "";
    if (c.parent) labels[key] = c.parent.summary || c.parent.id;
    (groups[key] = groups[key] || []).push(c);
  });
  const keys = Object.keys(groups).sort(function (a, b) { return (a === "" ? 1 : 0) - (b === "" ? 1 : 0) || (labels[a] || "").localeCompare(labels[b] || ""); });
  let body = "";
  if (!cards.length) body = '<div class="empty">' + (s === "doing" ? "Ничего не в работе" : "Пусто") + "</div>";
  else keys.forEach(function (k) {
    if (lanes && keys.length > 1) body += '<div class="lane">' + (k ? "↳ " + esc(labels[k]) : "без родителя") + "</div>";
    body += groups[k].map(function (c) { return boardCard(c, lanes); }).join("");
  });
  const more = s === "done" && col.total > cards.length;
  return '<section class="col" data-status="' + s + '" role="listitem" aria-labelledby="col-' + s + '">' +
    '<header><span id="col-' + s + '">' + STATUS_LABEL[s] + '</span><span class="n">' + (s === "done" && col.total > cards.length ? cards.length + " из " + col.total : col.total) + '</span><span class="grow"></span></header>' +
    '<div class="cards">' + body + (more ? '<button type="button" class="btn quiet sm" data-more="' + (doneLimit || DONE_LIMIT) * 2 + '">показать ещё</button>' : "") + "</div></section>";
}

async function renderBoard() {
  const el = $("#screen-board");
  const f = boardParams();
  const scope = state.scope;
  const seq = ++renderSeq;
  el.innerHTML = '<h2 id="h-board" class="sr-only">Доска</h2><p class="muted">Загрузка…</p>';
  let board, parentLabel = "";
  try {
    board = await api("/api/board" + qs({ scope: scope, type: f.type, parent: f.parent, tag: f.tag, doneLimit: f.doneLimit }));
    if (f.parent) {
      (board.columns || []).forEach(function (c) { (c.cards || []).forEach(function (x) { if (x.parent && x.parent.id === f.parent) parentLabel = x.parent.summary || ""; }); });
      if (!parentLabel) {
        try { parentLabel = title((await api("/api/memories/" + encodeURIComponent(f.parent))).memory); } catch (e) { parentLabel = f.parent; }
      }
    }
  } catch (err) {
    if (seq !== renderSeq) return;
    screenError(el, err, renderBoard);
    return;
  }
  if (seq !== renderSeq) return;
  const cols = board.columns || [];
  let n = 0, blocked = 0;
  const tags = {};
  cols.forEach(function (c) {
    (c.cards || []).forEach(function (x) { n++; if (x.blocked && c.status !== "done") blocked++; (x.tags || []).forEach(function (t) { tags[t] = 1; }); });
  });
  if (f.tag) tags[f.tag] = 1;
  const tagList = Object.keys(tags).sort();
  const hasFilter = f.type || f.parent || f.tag;
  el.innerHTML = '<h2 id="h-board" class="sr-only">Доска</h2>' +
    '<div class="board-toolbar">' +
    '<label>Тип <select id="b-type"><option value="">любой</option>' + Object.keys(TYPE_LABEL).map(function (k) { return '<option value="' + k + '"' + (f.type === k ? " selected" : "") + ">" + TYPE_LABEL[k] + "</option>"; }).join("") + "</select></label>" +
    '<span class="b-parent"><label for="b-parent">Внутри</label> ' + (f.parent
      ? '<span class="chip">' + esc(parentLabel.slice(0, 40)) + '<button type="button" data-board-parent="" aria-label="Снять фильтр по родителю">×</button></span>'
      : '<span class="picker inline"><input type="search" id="b-parent" placeholder="эпик или история…" autocomplete="off" data-picker="board-parent" aria-label="Внутри: поиск эпика или истории"><ul hidden></ul></span>') + "</span>" +
    '<label>Тег <select id="b-tag"><option value="">любой</option>' + tagList.map(function (t) { return '<option value="' + esc(t) + '"' + (f.tag === t ? " selected" : "") + ">" + esc(t) + "</option>"; }).join("") + "</select></label>" +
    '<label><input type="checkbox" id="b-lanes"' + (f.lanes ? " checked" : "") + "> дорожки по родителю</label>" +
    '<span class="muted" style="margin-left:auto">' + n + " " + plural(n, "задача", "задачи", "задач") + (blocked ? " · " + blocked + " заблокировано" : "") + " · " + esc(scope ? scopeShort(scope) : "все scope") + "</span>" +
    (hasFilter ? '<button type="button" class="btn quiet sm" data-board-reset>Сбросить</button>' : "") + "</div>" +
    '<p class="muted hint">Перетащи карточку в другую колонку или открой её и смени стадию — с клавиатуры: Enter на карточке, затем поле «Стадия». Заблокированную задачу дальше «к работе» не пустит сервер.</p>' +
    '<div class="board" role="list" aria-label="Стадии">' + cols.map(function (c) { return boardColumn(c, f.lanes, f.doneLimit); }).join("") + "</div>";
}

// moveCard — PATCH {status}: оптимистичный перенос карточки в колонку, затем
// перечитать доску; 409/400 → доска перечитывается (откат) + тост §5.
async function moveCard(id, status, summary) {
  const card = $('.card[data-id="' + id + '"]');
  if (!card || card.getAttribute("data-status") === status) return;
  const target = $('.col[data-status="' + status + '"] .cards');
  if (target) { const e = $(".empty", target); if (e) e.remove(); target.appendChild(card); }
  try {
    await api("/api/memories/" + encodeURIComponent(id), { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ status: status }) });
  } catch (err) {
    showToast(patchErrorText(err, { summary: summary }));
    refreshScreen();
    return;
  }
  showToast("Перенесена в «" + STATUS_LABEL[status] + "»");
  refreshScreen();
}

document.addEventListener("change", function (e) {
  const t = e.target;
  if (!(t instanceof Element)) return;
  if (t.id === "b-type") { setBoardParams({ type: t.value }); return; }
  if (t.id === "b-tag") { setBoardParams({ tag: t.value }); return; }
  if (t.id === "b-lanes") { setBoardParams({ lanes: t.checked }); return; }
  const mv = t.getAttribute("data-move");
  if (mv) { const c = t.closest(".card"); moveCard(mv, t.value, c ? c.getAttribute("aria-label") : ""); return; }
  const field = t.getAttribute("data-field");
  if (field && drawer.card) {
    const fields = {};
    fields[field] = t.value;
    patchRecord(fields, field === "status" ? "Стадия: " + STATUS_LABEL[t.value] : "", "#f-" + field).then(function (ok) { if (!ok) drawRecord(); });
  }
});

// drag & drop (мышь); клавиатурная альтернатива — select «Переместить» и поле «Стадия» в панели
let dragId = null;
document.addEventListener("dragstart", function (e) {
  const c = e.target instanceof Element ? e.target.closest(".card") : null;
  if (!c) return;
  dragId = c.getAttribute("data-id");
  c.classList.add("dragging");
  document.body.classList.add("dragging-any");
  e.dataTransfer.effectAllowed = "move";
  e.dataTransfer.setData("text/plain", dragId);
});
document.addEventListener("dragend", function (e) {
  document.body.classList.remove("dragging-any");
  const c = e.target instanceof Element ? e.target.closest(".card") : null;
  if (c) c.classList.remove("dragging");
  $$(".col.over").forEach(function (x) { x.classList.remove("over"); });
});
document.addEventListener("dragover", function (e) {
  const col = e.target instanceof Element ? e.target.closest(".col") : null;
  if (!col || !dragId) return;
  e.preventDefault();
  $$(".col.over").forEach(function (x) { if (x !== col) x.classList.remove("over"); });
  col.classList.add("over");
});
document.addEventListener("dragleave", function (e) {
  const col = e.target instanceof Element ? e.target.closest(".col") : null;
  if (col && !col.contains(e.relatedTarget)) col.classList.remove("over");
});
document.addEventListener("drop", function (e) {
  const col = e.target instanceof Element ? e.target.closest(".col") : null;
  if (!col || !dragId) return;
  e.preventDefault();
  const id = dragId; dragId = null;
  const c = $('.card[data-id="' + id + '"]');
  moveCard(id, col.getAttribute("data-status"), c ? c.getAttribute("aria-label") : "");
});

// --- пикер задачи (§2.4/§2.5): GET /api/search?q&mode=lexical&kind=task ---

let pickTimer = null;
document.addEventListener("input", function (e) {
  const t = e.target;
  if (!(t instanceof Element) || !t.hasAttribute("data-picker")) return;
  clearTimeout(pickTimer);
  const list = t.parentElement.querySelector("ul");
  const q = t.value.trim();
  if (!q) { list.hidden = true; list.innerHTML = ""; return; }
  pickTimer = setTimeout(async function () {
    let r;
    try {
      r = await api("/api/search" + qs({ q: q, mode: "lexical", kind: "task", scope: state.scope, limit: 10 }));
    } catch (err) { showToast(err.status === 0 ? "Сервер памяти не отвечает." : err.message); return; }
    if (t.value.trim() !== q) return;
    const items = (r.items || []).filter(function (m) { return m.id !== drawer.id && !m.supersededBy; });
    list.innerHTML = items.length ? items.map(function (m) {
      return '<li><button type="button" data-pick="' + esc(m.id) + '">' + esc(TYPE_LABEL[m.type] || m.type) + " · " + esc(title(m)) + " " + statusTag(m.status) + "</button></li>";
    }).join("") : '<li class="none">Ничего не нашлось</li>';
    list.hidden = false;
  }, 260);
});

function pickerClick(t) {
  const btn = t.closest("[data-pick]");
  if (!btn) return false;
  const input = btn.closest(".picker").querySelector("[data-picker]");
  const kind = input.getAttribute("data-picker"), id = btn.getAttribute("data-pick");
  if (kind === "board-parent") setBoardParams({ parent: id });
  else if (kind === "task-parent") patchRecord({ links: [{ id: id, rel: "part_of" }] }, "Привязана", '[data-picker="task-parent"]');
  else if (kind === "task-blocker") patchRecord({ links: [{ id: id, rel: "blocked_by" }] }, "Блокер добавлен", '[data-picker="task-blocker"]');
  return true;
}

// --- панель задачи (§2.5): поверх общей панели записи ---

function taskBody(card) {
  const t = card.memory, archived = !!t.supersededBy;
  const b = card.blockedBy || [], blocks = card.blocks || [], p = card.parent, ch = card.children || [], pr = card.progress;
  const other = outgoingLinks(card).concat(incomingLinks(card)).filter(function (x) { return x.rel !== "part_of" && x.rel !== "blocked_by"; });
  let html = "";
  if (archived) {
    html += '<div class="notice" style="margin-bottom:var(--s4)"><div class="grow">В архиве с ' + esc(rel(t.updatedAt)) + " — забыта. Архивные записи не редактируются.</div></div>";
  }
  html += '<h2 id="rec-title" class="summary task">' + (drawer.edit ? '<input type="text" id="ed-summary" aria-label="Заголовок" autocomplete="off" value="' + esc(t.summary || "") + '">' : esc(t.summary || t.id)) + "</h2>";
  if (b.length) {
    html += '<div class="notice danger" style="margin-bottom:var(--s4)"><div class="grow"><strong>Заблокирована.</strong> Дальше «к работе» не перевести, пока ' + (b.length === 1 ? "не закрыта" : "не закрыты") + ": " +
      b.map(function (x) { return '<button type="button" class="btn quiet sm inline-link" data-open="' + esc(x.id) + '">' + esc(x.summary || x.id) + "</button> (" + esc(STATUS_LABEL[x.status] || x.status) + ")"; }).join(", ") + "</div></div>";
  }
  const dis = archived ? " disabled" : "";
  html += '<div class="fields-2">' +
    '<div class="field"><label for="f-status">Стадия</label><select id="f-status" data-field="status"' + dis + ">" + STATUSES.map(function (s) { return '<option value="' + s + '"' + (s === t.status ? " selected" : "") + ">" + STATUS_LABEL[s] + "</option>"; }).join("") + "</select></div>" +
    '<div class="field"><label for="f-type">Тип</label><select id="f-type" data-field="type"' + dis + ">" + Object.keys(TYPE_LABEL).map(function (k) { return '<option value="' + k + '"' + (t.type === k ? " selected" : "") + ">" + TYPE_LABEL[k] + "</option>"; }).join("") + "</select></div>" +
    '<div class="field"><label for="f-branch">Ветка</label><input type="text" id="f-branch" class="mono" value="' + esc(t.branch || "") + '" placeholder="feat/…" autocomplete="off" spellcheck="false" data-field="branch"' + dis + "></div>" +
    '<div class="field"><label for="f-workflow">Процесс</label><input type="text" id="f-workflow" value="' + esc(t.workflow || "") + '" placeholder="dev, ultracode…" autocomplete="off" data-field="workflow"' + dis + "></div></div>" +
    '<div class="hgroup">' + imp(t.importance, !archived && !drawer.edit) + "<span>приоритет " + t.importance + " из 5</span><span class=\"muted\">·</span><span class=\"muted\">создана " + esc(rel(t.createdAt)) + "</span></div>" +
    '<div class="section"><h3>Описание</h3>' + (drawer.edit ? '<textarea id="ed-content" rows="10" aria-label="Описание">' + esc(t.content) + "</textarea>" : '<div class="prose">' + esc(t.content) + "</div>") + "</div>" +
    '<div class="section"><h3>Входит в</h3>' + (p
      ? '<ul class="links"><li><span class="rel">' + esc(TYPE_LABEL[p.type] || p.type) + '</span><span class="to"><button type="button" data-open="' + esc(p.id) + '">' + esc(p.summary || p.id) + "</button> " + statusTag(p.status) + "</span>" +
        (archived ? "<span></span>" : '<button type="button" class="btn quiet sm" data-unlink="' + esc(p.id) + '" data-rel="part_of">Отвязать</button>') + "</li></ul>"
      : '<p class="muted small">Ни в какой эпик или историю не входит.</p>') +
    (archived ? "" : '<div class="picker"><input type="search" placeholder="Найти эпик или историю…" aria-label="Родитель: поиск задачи" autocomplete="off" data-picker="task-parent"><ul hidden></ul></div>') + "</div>";
  if (ch.length) {
    html += '<div class="section"><h3>Дочерние <span class="muted">' + (pr ? pr.done + " из " + pr.total + " готово" : ch.length) + "</span></h3>" +
      (pr && pr.total ? '<div class="progress" style="margin-bottom:6px"><i style="width:' + Math.round(pr.done / pr.total * 100) + '%"></i></div>' : "") +
      '<ul class="links">' + ch.map(function (c) { return '<li><span class="rel in">включает</span><span class="to"><button type="button" data-open="' + esc(c.id) + '">' + esc(c.summary || c.id) + "</button> " + statusTag(c.status) + "</span><span></span></li>"; }).join("") + "</ul></div>";
  }
  html += '<div class="section"><h3>Блокировки</h3>' + (b.length || blocks.length ? '<ul class="links">' +
    b.map(function (x) { return '<li><span class="rel out">заблокирована</span><span class="to"><button type="button" data-open="' + esc(x.id) + '">' + esc(x.summary || x.id) + "</button> " + statusTag(x.status) + "</span>" + (archived ? "<span></span>" : '<button type="button" class="btn quiet sm" data-unlink="' + esc(x.id) + '" data-rel="blocked_by">Снять</button>') + "</li>"; }).join("") +
    blocks.map(function (x) { return '<li><span class="rel in">блокирует</span><span class="to"><button type="button" data-open="' + esc(x.id) + '">' + esc(x.summary || x.id) + "</button> " + statusTag(x.status) + "</span><span></span></li>"; }).join("") + "</ul>"
    : '<p class="muted small">Ничем не заблокирована и ничего не блокирует.</p>') +
    (archived ? "" : '<div class="picker"><input type="search" placeholder="Добавить блокер: найти задачу…" aria-label="Блокер: поиск задачи" autocomplete="off" data-picker="task-blocker"><ul hidden></ul></div>') + "</div>";
  if (other.length) html += '<div class="section"><h3>Связано</h3><ul class="links">' + other.map(function (x) { return linkRow(x); }).join("") + "</ul></div>";
  html += '<div class="section"><h3>Теги</h3>' + tagsEditor(t) + "</div>" + serviceMeta(card) + (drawer.confirm ? forgetConfirm(card) : "");
  return html;
}

const screens = {
  overview: { render: renderOverview },
  memory: { render: renderMemory },
  board: { render: renderBoard },
};

let lastScreenKey = "";  // раздел+scope+фильтры последней отрисовки: смена одного id панели экран не перерисовывает

function renderView() {
  Object.keys(screens).forEach(function (v) { $("#screen-" + v).hidden = v !== state.view; });
  // доске нужна ширина под 7 колонок; остальные экраны — не шире 1400 px
  $("#main").classList.toggle("wide", state.view === "board");
  const key = state.view + "|" + state.scope + "|" + state.params.toString();
  if (key === lastScreenKey) {
    // открытие/закрытие панели: список остаётся (и элемент, к которому вернётся фокус, тоже)
    $$(".row[data-open]").forEach(function (r) { r.setAttribute("aria-selected", String(r.getAttribute("data-open") === state.id)); });
    return;
  }
  lastScreenKey = key;
  screens[state.view].render();
}

// refreshScreen — перечитать экран под панелью после PATCH/DELETE (данные изменились).
function refreshScreen() {
  lastScreenKey = "";
  renderView();
}

// --- панель записи (drawer, §2.3, задача 16.5) ---

// drawer — состояние открытой панели: карточка /api/memories/{id}, режим
// правки и инлайн-подтверждение «Забыть» (в хэш не пишутся — это не адрес).
const drawer = { id: "", card: null, edit: false, confirm: false };
let drawerFocus = "";   // селектор элемента, которому вернуть фокус после перерисовки тела

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
    if (!drawer.id && !root.firstChild) return;
    drawer.id = ""; drawer.card = null; drawer.edit = false; drawer.confirm = false;
    root.innerHTML = "";
    setBackgroundInert(false);
    // возврат фокуса: на открывший элемент, если он ещё в документе, иначе на main
    const back = lastFocus && document.contains(lastFocus) && lastFocus !== document.body ? lastFocus : $("#main");
    lastFocus = null;
    back.focus();
    return;
  }
  if (state.id === drawer.id) return;
  drawer.id = state.id; drawer.card = null; drawer.edit = false; drawer.confirm = false;
  setBackgroundInert(true);
  root.innerHTML =
    '<div class="drawer-backdrop" data-close></div>' +
    '<aside class="drawer" role="dialog" aria-modal="true" aria-labelledby="rec-title" tabindex="-1">' +
    '<header><span id="rec-kind"></span><div class="grow"><span class="muted" id="rec-scope"></span></div>' +
    '<span id="rec-extra"></span>' +
    '<button type="button" class="btn quiet sm" data-close aria-label="Закрыть панель">Закрыть ✕</button></header>' +
    '<div class="content" id="rec-content"><h2 id="rec-title" class="summary">Запись</h2><p class="muted">Загрузка…</p></div>' +
    '<div class="actions" id="rec-actions" hidden></div>' +
    "</aside>";
  $(".drawer", root).focus();
  loadRecord(state.id);
}

// loadRecord — GET /api/memories/{id} → drawRecord. Ответ на уже закрытую или
// сменившуюся панель отбрасывается.
async function loadRecord(id) {
  let card;
  try {
    card = await api("/api/memories/" + encodeURIComponent(id));
  } catch (err) {
    if (drawer.id !== id || !$("#rec-content")) return;
    const body = $("#rec-content");
    body.innerHTML = '<h2 id="rec-title" class="summary">Запись</h2>';
    if (err.status === 404) {
      body.innerHTML += '<div class="notice warn">Записи <code>' + esc(id) + "</code> нет — удалена или id указан с ошибкой.</div>";
    } else {
      body.appendChild(apiErrorNotice(err.path, err, function () { loadRecord(id); }));
    }
    return;
  }
  if (drawer.id !== id || !$("#rec-content")) return;
  drawer.card = card;
  drawRecord();
}

// drawRecord — шапка, тело и футер панели по карточке; после перерисовки —
// фокус на элемент из drawerFocus (тег-поле, сегмент важности, кнопка).
function drawRecord() {
  const card = drawer.card, m = card.memory;
  const isTask = m.kind === "task";
  const archived = !!m.supersededBy;
  $("#rec-kind").innerHTML = kindTag(m.kind);
  $("#rec-scope").textContent = m.scope;
  $("#rec-extra").innerHTML = isTask ? '<button type="button" class="btn quiet sm" data-goto="board">К доске</button>' : "";
  $(".drawer").classList.toggle("editing", drawer.edit);
  $("#rec-content").innerHTML = isTask ? taskBody(card) : memoryBody(card);
  const actions = $("#rec-actions");
  actions.hidden = archived;
  actions.innerHTML = archived ? "" : (drawer.edit
    ? '<button type="button" class="btn primary" data-save>Сохранить</button><button type="button" class="btn" data-cancel>Отменить</button>'
    : '<button type="button" class="btn" data-edit>Изменить</button><span class="grow"></span><button type="button" class="btn danger" data-forget-ask>Забыть…</button>');
  if (drawerFocus) {
    const f = $(drawerFocus, $("#drawer-root"));
    drawerFocus = "";
    if (f) f.focus();
  }
}

// incomingLinks — входящие связи из neighbors: у кого dst == id (у DTO записи
// только исходящие links).
function incomingLinks(card) {
  const id = card.memory.id, out = [];
  (card.neighbors || []).forEach(function (n) {
    (n.links || []).forEach(function (l) { if (l.dst === id) out.push({ to: n, rel: l.rel, dir: "in" }); });
  });
  return out;
}

// outgoingLinks — исходящие links записи; цель берётся из neighbors (тумстоуны
// в neighbors не приходят — тогда показываем только id).
function outgoingLinks(card) {
  const byId = {};
  (card.neighbors || []).forEach(function (n) { byId[n.id] = n; });
  return (card.memory.links || []).map(function (l) { return { to: byId[l.dst] || { id: l.dst }, rel: l.rel, dir: "out" }; });
}

function linkRow(x, action) {
  const t = x.to, lab = x.dir === "out" ? REL_LABEL[x.rel] : REL_IN_LABEL[x.rel];
  return '<li><span class="rel ' + x.dir + '">' + esc(lab || x.rel) + '</span><span class="to">' + (t.kind ? kindTag(t.kind) : "") +
    '<button type="button" data-open="' + esc(t.id) + '">' + (t.kind ? esc(title(t)) : "<code>" + esc(t.id) + "</code>") + "</button>" +
    (t.kind === "task" && t.status ? " " + statusTag(t.status) : "") + "</span>" + (action || "<span></span>") + "</li>";
}

function tagsEditor(m) {
  if (m.supersededBy) return '<div class="tags-edit">' + (m.tags && m.tags.length ? tagChips(m.tags) : '<span class="muted">тегов нет</span>') + "</div>";
  return '<div class="tags-edit">' + (m.tags || []).map(function (t) {
    return '<span class="chip tag">' + esc(t) + '<button type="button" data-untag="' + esc(t) + '" aria-label="Убрать тег ' + esc(t) + '">×</button></span>';
  }).join("") + '<input type="text" placeholder="+ тег…" autocomplete="off" spellcheck="false" aria-label="Новый тег" data-newtag></div>';
}

// forgetConfirm — инлайн-подтверждение (§5): число связей, для задачи с детьми — предупреждение.
function forgetConfirm(card) {
  const m = card.memory;
  const n = (m.links || []).length + incomingLinks(card).length;
  const kids = (card.children || []).length;
  return '<div class="confirm" role="alertdialog" aria-labelledby="fc-h"><h3 id="fc-h">Забыть эту ' + (m.kind === "task" ? "задачу" : "запись") + "?</h3>" +
    "<p>Она выпадет из recall агента и уйдёт в архив; текст и " + (n ? n + " " + plural(n, "связь", "связи", "связей") : "связи") + " сохранятся, восстановить можно через <code>memory_update</code>. " +
    (kids ? "<strong>" + kids + " " + plural(kids, "дочерняя задача останется", "дочерние задачи останутся", "дочерних задач останутся") + " без родителя.</strong>" : "") + "</p>" +
    '<div class="btns"><button type="button" class="btn danger solid" data-forget>Забыть</button><button type="button" class="btn" data-forget-cancel>Оставить</button></div></div>';
}

// serviceMeta — «Служебное»: id, полные даты, чтения, источник, вектор.
function serviceMeta(card) {
  const m = card.memory, e = card.embedding || {};
  return '<div class="section"><h3>Служебное</h3><dl class="meta-grid">' +
    "<dt>id</dt><dd><code>" + esc(m.id) + "</code></dd><dt>scope</dt><dd>" + esc(m.scope) + "</dd>" +
    "<dt>создана</dt><dd>" + esc(fmtDate(m.createdAt)) + "</dd><dt>изменена</dt><dd>" + esc(fmtDate(m.updatedAt)) + "</dd>" +
    "<dt>читалась</dt><dd>" + (m.accessedAt ? esc(fmtDate(m.accessedAt)) + " · " + m.accessCount + " " + plural(m.accessCount, "раз", "раза", "раз") : "ещё ни разу") + "</dd>" +
    "<dt>источник</dt><dd>" + (m.source ? "<code>" + esc(m.source) + "</code>" : "—") + "</dd>" +
    (m.expiresAt ? "<dt>истекает</dt><dd>" + esc(fmtDate(m.expiresAt)) + "</dd>" : "") +
    "<dt>вектор</dt><dd>" + (e.embedded ? esc(e.model || "есть") + (e.dim ? " · " + e.dim : "") : "в очереди на эмбеддинг") + "</dd></dl></div>";
}

// memoryBody — тело панели для факта/события/процедуры/долга (§2.3).
function memoryBody(card) {
  const m = card.memory, archived = !!m.supersededBy, e = card.embedding || {};
  const links = outgoingLinks(card).concat(incomingLinks(card));
  const sem = card.semanticNeighbors || [];
  let html = "";
  if (archived) {
    const self = m.supersededBy === m.id;
    html += '<div class="notice" style="margin-bottom:var(--s4)"><div class="grow">В архиве с ' + esc(rel(m.updatedAt)) +
      (self ? " — забыта" : ' — заменена записью <button type="button" class="btn quiet sm" data-open="' + esc(m.supersededBy) + '"><code>' + esc(m.supersededBy) + "</code></button>") +
      ". Архивные записи не редактируются.</div></div>";
  }
  if (m.expiresAt && !archived) {
    html += '<div class="notice warn" style="margin-bottom:var(--s4)"><div class="grow">Истекает ' + esc(rel(m.expiresAt)) + " (" + esc(fmtDate(m.expiresAt).slice(0, 10)) + ") — после этого выпадет из recall и уйдёт в архив.</div></div>";
  }
  html += '<h2 id="rec-title" class="sr-only">Запись: ' + esc(title(m)) + "</h2>" +
    '<div class="hgroup">' + imp(m.importance, !archived && !drawer.edit) + "<span>" + m.importance + " из 5</span>" +
    '<span class="muted">·</span><span class="muted">создана ' + esc(rel(m.createdAt)) + "</span></div>";
  if (drawer.edit) {
    html += '<div class="field"><label for="ed-summary">Кратко (summary)</label><input type="text" id="ed-summary" autocomplete="off" value="' + esc(m.summary || "") + '"></div>' +
      '<div class="field"><label for="ed-content">Полный текст</label><textarea id="ed-content" rows="10">' + esc(m.content) + "</textarea></div>";
  } else {
    html += (m.summary ? '<p class="summary">' + esc(m.summary) + "</p>" : "") + '<div class="prose">' + esc(m.content) + "</div>";
  }
  html += '<div class="section"><h3>Теги</h3>' + tagsEditor(m) + "</div>" +
    '<div class="section"><h3>Связи <span class="muted">' + links.length + "</span></h3>" +
    (links.length ? '<ul class="links">' + links.map(function (x) { return linkRow(x); }).join("") + "</ul>"
      : '<p class="muted small">Связей нет. Их добавляет агент через <code>memory_update</code>, здесь — только просмотр.</p>') + "</div>" +
    '<div class="section"><h3>Похожие по вектору <span class="muted">' + (e.embedded ? "cosine" + (e.model ? ", " + esc(e.model) : "") : "нет данных — эмбеддер недоступен или запись в очереди") + "</span></h3>" +
    (!e.embedded ? "" : sem.length ? '<ul class="links">' + sem.map(function (n) {
      return '<li><span class="rel">' + Number(n.distance).toFixed(2) + '</span><span class="to">' + kindTag(n.kind) + '<button type="button" data-open="' + esc(n.id) + '">' + esc(title(n)) + "</button></span><span></span></li>";
    }).join("") + "</ul>" : '<p class="muted small">Близких по смыслу записей нет.</p>') + "</div>" +
    serviceMeta(card) + (drawer.confirm ? forgetConfirm(card) : "");
  return html;
}

// patchRecord — PATCH /api/memories/{id}; успех → перечитать карточку, экран под
// панелью и тост; ошибка → тост с текстом сервера (§5), состояние не меняется.
async function patchRecord(fields, okText, focusSel) {
  const id = drawer.id;
  try {
    await api("/api/memories/" + encodeURIComponent(id), { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify(fields) });
  } catch (err) {
    showToast(patchErrorText(err, drawer.card && drawer.card.memory));
    return false;
  }
  if (okText) showToast(okText);
  if (drawer.id === id) { drawerFocus = focusSel || ""; await loadRecord(id); }
  refreshScreen();
  return true;
}

// patchErrorText — тексты ошибок PATCH (§5): 409 с блокерами, прочее — сообщение сервера.
function patchErrorText(err, m) {
  if (err.status === 0) return "Сервер памяти не отвечает — изменение не сохранено.";
  if (err.status === 409 && err.body && err.body.blockers && err.body.blockers.length) {
    const t = m ? (m.summary || m.content || "").slice(0, 40) : "";
    return "Не переведена: «" + t + "…» заблокирована — " + err.body.blockers.map(function (b) { return (b.summary || b.id).slice(0, 30); }).join(", ");
  }
  return err.message;
}

async function forgetRecord() {
  const id = drawer.id, m = drawer.card.memory;
  try {
    await api("/api/memories/" + encodeURIComponent(id), { method: "DELETE" });
  } catch (err) {
    showToast(err.status === 0 ? "Сервер памяти не отвечает — запись не забыта." : err.message);
    return;
  }
  showToast("Забыта: «" + title(m).slice(0, 50) + "» — в архиве");
  closeRecord();
  refreshScreen();
}

// drawerClick — действия внутри панели; true — событие обработано.
function drawerClick(t) {
  if (!drawer.card) return false;
  const m = drawer.card.memory;
  if (t.closest("[data-edit]")) { drawer.edit = true; drawer.confirm = false; drawerFocus = "#ed-content"; drawRecord(); return true; }
  if (t.closest("[data-cancel]")) { drawer.edit = false; drawerFocus = "[data-edit]"; drawRecord(); return true; }
  if (t.closest("[data-save]")) {
    const s = $("#ed-summary"), c = $("#ed-content");
    const fields = {};
    if (c) fields.content = c.value;
    if (s) fields.summary = s.value.trim();
    drawer.edit = false;
    patchRecord(fields, "Сохранено", "[data-edit]").then(function (ok) { if (!ok) { drawer.edit = true; drawRecord(); } });
    return true;
  }
  if (t.closest("[data-forget-ask]")) { drawer.confirm = true; drawerFocus = "[data-forget]"; drawRecord(); return true; }
  if (t.closest("[data-forget-cancel]")) { drawer.confirm = false; drawerFocus = "[data-forget-ask]"; drawRecord(); return true; }
  if (t.closest("[data-forget]")) { forgetRecord(); return true; }
  const ul = t.closest("[data-unlink]");
  if (ul) {
    const rel_ = ul.getAttribute("data-rel");
    patchRecord({ unlinks: [{ id: ul.getAttribute("data-unlink"), rel: rel_ }] }, rel_ === "blocked_by" ? "Блокировка снята" : "Отвязано", '[data-picker="' + (rel_ === "blocked_by" ? "task-blocker" : "task-parent") + '"]');
    return true;
  }
  const untag = t.closest("[data-untag]");
  if (untag) {
    const tag = untag.getAttribute("data-untag");
    patchRecord({ tags: (m.tags || []).filter(function (x) { return x !== tag; }) }, "Тег «" + tag + "» снят", "[data-newtag]");
    return true;
  }
  const ib = t.closest("[data-imp]");
  if (ib) {
    const n = parseInt(ib.getAttribute("data-imp"), 10);
    patchRecord({ importance: n }, "Важность " + n + " из 5", '[data-imp="' + n + '"]');
    return true;
  }
  return false;
}

// addTag — Enter в поле «+ тег…»: PATCH {tags} заменяющей семантикой.
function addTag(input) {
  const m = drawer.card && drawer.card.memory;
  if (!m) return;
  const v = input.value.trim();
  if (!v) return;
  if ((m.tags || []).indexOf(v) !== -1) { input.value = ""; return; }
  patchRecord({ tags: (m.tags || []).concat([v]) }, "Тег «" + v + "» добавлен", "[data-newtag]");
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
  if (pickerClick(t)) return;
  if (state.id && t.closest("#drawer-root") && drawerClick(t)) return;
  if (t.closest("[data-board-reset]")) { setBoardParams({ type: "", parent: "", tag: "" }); return; }
  const bp = t.closest("[data-board-parent]");
  if (bp) { setBoardParams({ parent: bp.getAttribute("data-board-parent") }); return; }
  const more = t.closest("[data-more]");
  if (more) { setBoardParams({ doneLimit: parseInt(more.getAttribute("data-more"), 10) || 0 }); return; }
  const goto = t.closest("[data-goto]");
  if (goto) { navigate({ view: goto.getAttribute("data-goto"), params: new URLSearchParams(), id: "" }); return; }
  if (t.closest("[data-close]")) { closeRecord(); return; }
  const open = t.closest("[data-open]");
  if (open) { openRecord(open.getAttribute("data-open")); return; }
  const gotoKind = t.closest("[data-goto-kind]");
  if (gotoKind) { navigate({ view: "memory", params: new URLSearchParams({ kind: gotoKind.getAttribute("data-goto-kind") }), id: "" }); return; }
  const sc = t.closest("[data-scope]");
  if (sc) { navigate({ scope: sc.getAttribute("data-scope") }); return; }
  const mode = t.closest("[data-mode]");
  if (mode) { focusSearch = true; setMemoryParams({ mode: mode.getAttribute("data-mode") }); return; }
  if (t.closest("[data-reset]")) {
    // «Сбросить всё» (§2.2): вид, архив, запрос и scope; режим поиска остаётся
    const p = new URLSearchParams();
    if (memoryParams().mode === "lexical") p.set("mode", "lexical");
    focusSearch = true;
    navigate({ scope: "", params: p });
    return;
  }
  const page = t.closest("[data-page]");
  if (page) { setMemoryParams({ offset: parseInt(page.getAttribute("data-page"), 10) || 0 }); return; }
  if (t.closest(".skip")) {
    // skip link не должен менять хэш (он — маршрут): просто переводим фокус в main
    e.preventDefault();
    $("#main").focus();
  }
});

document.addEventListener("keydown", function (e) {
  if (e.key === "Escape" && state.id) {
    e.preventDefault();
    // Esc снимает подтверждение или правку, и только потом закрывает панель
    if (drawer.confirm) { drawer.confirm = false; drawerFocus = "[data-forget-ask]"; drawRecord(); }
    else if (drawer.edit) { drawer.edit = false; drawerFocus = "[data-edit]"; drawRecord(); }
    else closeRecord();
    return;
  }
  if (e.key === "Enter" && e.target.matches("[data-newtag]")) { e.preventDefault(); addTag(e.target); return; }
  // «/» — в поле поиска (§2.2); из другого раздела — сначала в «Память»
  if (e.key === "/" && !state.id && !e.target.matches("input,textarea,select") && !e.ctrlKey && !e.metaKey && !e.altKey) {
    e.preventDefault();
    focusSearch = true;
    if (state.view !== "memory") navigate({ view: "memory", params: new URLSearchParams(), id: "" });
    else { const i = $("#q"); if (i) { focusSearch = false; i.focus(); i.select(); } }
  }
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
