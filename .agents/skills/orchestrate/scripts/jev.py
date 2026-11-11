#!/usr/bin/env python3
"""Суждения Jev для orchestrate: Jev отвечает на типизированные вопросы, решение
принимает таблица в этом файле. Только стандартная библиотека Python >= 3.9.

Команды (вывод — одна строка JSON в stdout):
  config                      найденная конфигурация без сети и без ключа
  check                       GET /v1/models без инференса: куда реально уходят запросы
  calibrate --state FILE|-    профиль модели для назначения (references/jev.md)
  repair-cause --state FILE|- действие после неудачного repair

Коды выхода: 0 ответ; 2 неверный вход; 3 Jev не настроен; 4 сеть, 5xx или
некорректный ответ; 5 ошибка API 4xx (ключ, баланс, лимит).

Ключ: переменная окружения, иначе .env в корне основного checkout проекта.
Ключ не печатается и не пишется никуда, кроме заголовка Authorization.
"""

import argparse
import http.client
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

PROVIDERS = {
    # Порядок важен: Jev AI — основной провайдер проекта, TypeSafe — официальный.
    "jev-ai": {"key": "JEV_AI_API_KEY", "base": "JEV_AI_BASE_URL", "model": "JEV_AI_MODEL",
               "default_base": "https://jev-ai.pro/api"},
    "typesafe": {"key": "TYPESAFE_API_KEY", "base": "TYPESAFE_BASE_URL",
                 "model": "TYPESAFE_DEFAULT_MODEL", "default_base": "https://api.typesafe.ai"},
}
DEFAULT_MODEL = "jev-latest"
TIMEOUT_S = 10
RETRY_STATUSES = {408, 429}
MAX_STATE_CHARS = 60000  # с запасом под контекст 32k токенов вместе с вопросами
PROFILES = ("light", "coding", "top")
EFFORTS = ("low", "medium", "high", "max")  # уровни усилия; значения клиента — по model-selection.md
# Политика проекта для top: table — по таблицам; escalation_only — top только эскалацией.
TOP_POLICIES = ("table", "escalation_only")
ROLES = ("scout", "planner", "dev-lead", "worker", "integrator", "verifier")
DECIDING_ROLES = ("planner", "dev-lead", "integrator", "verifier")
REVIEW_RISKS = ("low", "medium", "high", "unknown")
YES, NO = 0.7, 0.3  # пороги noul: >= YES — да, <= NO — нет, между — неясно
LARGE = 0.3         # P(T2 или T3), начиная с которой задача считается крупной
CAUSE_SURE = 0.7


class Fail(Exception):
    def __init__(self, code, error, message, **extra):
        super().__init__(message)
        self.code, self.error, self.extra = code, error, extra


# ---------- вопросы: вся формулировка — только здесь ----------

def noul_q(instructions, true, false):
    return {"type": "noul", "instructions": instructions,
            "criteria": {"true": true, "false": false}}


CALIBRATE_QUESTIONS = {
    "size": {
        "type": "score",
        "instructions": "How large is the change described in `task`?",
        "criteria": [
            "A typo, a constant, a message text, or a local fix that changes no contract",
            "A local change inside the existing structure: a new field, a new variant of an "
            "existing kind, or an adjusted rule in one place",
            "A new feature: a new use case, a new integration or adapter, or changes across "
            "several modules",
            "A new system or subsystem, a cross-cutting change, or a large refactoring",
        ],
    },
    "ambiguous": noul_q(
        "Can `task.goal` reasonably be read in more than one way that leads to different work?",
        "Two competent developers could build materially different things from `task`",
        "`task` states one concrete outcome and how it will be checked"),
    "design_choice": noul_q(
        "Does `task` require choosing an architecture, a boundary, or a technology "
        "that is not decided yet?",
        "Someone has to pick between design options before the work can be done",
        "`task` only applies decisions that are already made"),
    "auth": noul_q(
        "Does `task` change authentication or access permissions?",
        "Login, tokens, roles, or access checks change",
        "Who can access what stays exactly the same"),
    "secrets": noul_q(
        "Does `task` handle secrets such as keys, passwords, or tokens?",
        "Secrets are read, stored, passed, or logged by the changed code",
        "No secret value is involved"),
    "external_input": noul_q(
        "Does `task` change how input from outside the system is parsed or trusted?",
        "Requests, uploads, or third-party data are parsed or validated differently",
        "Only internal data that the system itself produced is involved"),
    "migration": noul_q(
        "Does `task` change data the application stores while it runs, or its schema?",
        "Existing database records, user files, or caches that the running application "
        "keeps must be changed or converted",
        "The data the application keeps stays as it is; editing source code or "
        "documentation files is not a change to stored data"),
    "public_contract": noul_q(
        "Does `task` change a public API, file format, or protocol that others depend on?",
        "Callers, clients, or users outside the changed code must adapt",
        "Only internal code changes; every external interface stays the same"),
}
RISK_QUESTIONS = ("auth", "secrets", "external_input", "migration", "public_contract")
OPEN_QUESTIONS = ("ambiguous", "design_choice")

CAUSES = {
    "brief_gap": "The task description was missing information, contradicted itself, "
                 "or asked for the wrong thing",
    "environment": "A tool, dependency, network, or runtime in the environment failed "
                   "or was missing",
    "permissions": "An action was refused because access or permission was not granted",
    "missing_data": "Required input data, files, or answers were not available",
    "reasoning_error": "The information and tools were sufficient, but the agent's logic "
                       "or understanding of the domain was wrong",
}
REPAIR_QUESTIONS = {
    "cause": {"type": "choice",
              "instructions": "What is the main reason the work in `failure_report` failed?",
              "criteria": CAUSES},
}
BLOCKER_CAUSES = ("environment", "permissions", "missing_data")


# ---------- конфигурация ----------

def parse_dotenv(path):
    values = {}
    for raw in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        if line.startswith("export "):
            line = line[len("export "):].lstrip()
        name, value = line.split("=", 1)
        name, value = name.strip(), value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        elif " #" in value:
            value = value.split(" #", 1)[0].rstrip()
        values[name] = value
    return values


def project_root(explicit):
    if explicit:
        return Path(explicit).resolve()
    try:
        common = subprocess.run(
            ["git", "rev-parse", "--git-common-dir"],
            capture_output=True, text=True, timeout=10, check=True).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return Path.cwd().resolve()
    # Старый git отдаёт путь относительно cwd; --path-format появился только в 2.31.
    common_path = (Path.cwd() / common).resolve()
    # Основной checkout — родитель общего .git; у bare-репозитория его нет.
    if common_path.name == ".git":
        return common_path.parent.resolve()
    return Path.cwd().resolve()


def load_config(project):
    root = project_root(project)
    env_file = root / ".env"
    dotenv = {}
    if env_file.is_file():
        try:
            dotenv = parse_dotenv(env_file)
        except (OSError, UnicodeDecodeError) as exc:
            raise Fail(3, "bad_config", "cannot read %s: %s" % (env_file, type(exc).__name__))

    sources = {"env": os.environ, ".env": dotenv}

    for provider, names in PROVIDERS.items():
        source = next((s for s in ("env", ".env") if sources[s].get(names["key"])), None)
        if not source:
            continue
        # Адрес и модель — только из того же источника, что и ключ: иначе ключ
        # из окружения ушёл бы на адрес, записанный в .env чужого репозитория.
        values = sources[source]
        key = values[names["key"]]
        base = (values.get(names["base"]) or names["default_base"]).rstrip("/")
        model = values.get(names["model"]) or DEFAULT_MODEL
        parsed = urllib.parse.urlparse(base)
        local = parsed.hostname in LOCAL_HOSTS
        if parsed.scheme != "https" and not (parsed.scheme == "http" and local):
            raise Fail(3, "bad_config",
                       "%s must be https:// (http:// only for localhost)" % names["base"])
        return {"provider": provider, "key": key, "key_source": source, "base_url": base,
                "host": parsed.hostname, "model": model,
                "env_file": str(env_file) if env_file.is_file() else None}
    raise Fail(3, "not_configured",
               "no JEV_AI_API_KEY or TYPESAFE_API_KEY in environment or %s" % env_file,
               env_file=str(env_file))


def public(config):
    return {k: v for k, v in config.items() if k != "key"}


# ---------- HTTP ----------

def retry_delays():
    raw = os.environ.get("JEV_AI_RETRY_DELAYS")
    if raw is None:
        return [1.0, 3.0]
    return [float(x) for x in raw.split(",") if x.strip()]


def redact(text, key):
    return text.replace(key, "***") if key else text


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """Редирект не выполняется: urllib перенёс бы Authorization на новый адрес."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


OPENER = urllib.request.build_opener(NoRedirect)
# Локальный http:// — мимо прокси: иначе ключ ушёл бы прокси открытым текстом.
LOCAL_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
LOCAL_HOSTS = ("127.0.0.1", "localhost", "::1")


def call(config, method, path, payload=None):
    url = config["base_url"] + path
    data = None if payload is None else json.dumps(payload, ensure_ascii=False).encode("utf-8")
    headers = {"Authorization": "Bearer " + config["key"], "Accept": "application/json",
               "User-Agent": "orchestrate-jev/1"}
    if data is not None:
        headers["Content-Type"] = "application/json"
    delays = retry_delays()
    attempt = 0
    while True:
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        retry_after = None
        try:
            opener = LOCAL_OPENER if config.get("host") in LOCAL_HOSTS else OPENER
            with opener.open(request, timeout=TIMEOUT_S) as response:
                body = response.read().decode("utf-8")
            try:
                return json.loads(body)
            except ValueError:
                raise Fail(4, "bad_response", "response is not JSON")
        except urllib.error.HTTPError as exc:
            status = exc.code
            detail = redact(exc.read().decode("utf-8", "replace")[:300], config["key"])
            if status not in RETRY_STATUSES and status < 500:
                raise Fail(5, "api_error", "API returned %d" % status,
                           status=status, detail=detail)
            last = Fail(4, "unavailable", "API returned %d" % status,
                        status=status, detail=detail)
            retry_after = exc.headers.get("Retry-After") if exc.headers else None
        except (urllib.error.URLError, http.client.HTTPException, OSError) as exc:
            reason = getattr(exc, "reason", exc)
            last = Fail(4, "unavailable", redact("network error: %s" % reason, config["key"]))
        if attempt >= len(delays):
            raise last
        delay = delays[attempt]
        if retry_after and retry_after.isdigit() and delays[attempt] > 0:
            delay = min(float(retry_after), 10.0)
        time.sleep(delay)
        attempt += 1


def system_one(config, state, questions, response_file):
    payload = {"state": state, "questions": questions, "model": config["model"]}
    if response_file:
        try:
            result = json.loads(Path(response_file).read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            raise Fail(2, "bad_input", "cannot read response file: %s" % type(exc).__name__)
    else:
        result = call(config, "POST", "/v1/systemone", payload)
    answers = result.get("answers") if isinstance(result, dict) else None
    if not isinstance(answers, dict) or set(questions) - set(answers):
        raise Fail(4, "bad_response", "response lacks answers for some questions")
    return result


# ---------- вход ----------

def read_state(source):
    try:
        text = sys.stdin.read() if source == "-" else Path(source).read_text(encoding="utf-8")
        state = json.loads(text)
    except (OSError, ValueError) as exc:
        raise Fail(2, "bad_input", "cannot read state JSON: %s" % type(exc).__name__)
    if not isinstance(state, dict):
        raise Fail(2, "bad_input", "state must be a JSON object")
    return state


def require(state, name, kind, allowed=None):
    value = state.get(name)
    if not isinstance(value, kind) or (kind is str and not value.strip()):
        raise Fail(2, "bad_input", "field %r is required" % name)
    if allowed is not None and value not in allowed:
        raise Fail(2, "bad_input", "field %r must be one of %s" % (name, ", ".join(allowed)))
    return value


def string_list(state, name):
    value = state.get(name, [])
    if not isinstance(value, list) or not all(isinstance(x, str) for x in value):
        raise Fail(2, "bad_input", "field %r must be a list of strings" % name)
    return value


def check_size(state):
    if len(json.dumps(state, ensure_ascii=False)) > MAX_STATE_CHARS:
        raise Fail(2, "state_too_large",
                   "state exceeds %d characters; shorten it" % MAX_STATE_CHARS)


def verdict(p):
    return "yes" if p >= YES else "no" if p <= NO else "unclear"


def prob(answer, field):
    value = answer.get(field) if isinstance(answer, dict) else None
    if not isinstance(value, (int, float)) or isinstance(value, bool) or not 0 <= value <= 1:
        raise Fail(4, "bad_response", "answer field %r is not a probability" % field)
    return float(value)


# ---------- таблицы решений ----------

def decide_profile(facts, judgments):
    """Профиль назначения. Пол из фактов Jev не понижает; неясный риск — как высокий.

    Риск поднимает только тех, кто пишет, решает или принимает. Read-only сбор фактов
    ничего не решает (правило 6 model-selection), поэтому риск задачи его не поднимает.
    """
    rules = []
    level = 0
    role = facts["role"]
    risk_applies = not facts["read_only"] or role in DECIDING_ROLES

    def lift(target, rule):
        nonlocal level
        rules.append(rule)
        level = max(level, PROFILES.index(target))

    if facts["writes_code"]:
        lift("coding", "writes_code_floor")
    if not risk_applies:
        rules.append("read_only_risk_not_applied")
    elif facts["review_md_risk"] == "high":
        lift("top", "review_md_high_floor")
    elif facts["review_md_risk"] == "unknown":
        lift("top", "review_md_unknown_as_high")
    for name in RISK_QUESTIONS if risk_applies else ():
        v = judgments[name]["verdict"]
        if v == "yes":
            lift("top", "risk_signal:" + name)
        elif v == "unclear":
            lift("top", "risk_unclear:" + name)
    large = judgments["size"]["p_large"] >= LARGE
    open_ = any(judgments[name]["verdict"] != "no" for name in OPEN_QUESTIONS)
    if large or open_:
        lift("top" if role in DECIDING_ROLES else "coding", "large_or_open")
    elif role in DECIDING_ROLES:
        lift("coding", "deciding_role")
    else:
        rules.append("small_and_clear")
    author = facts.get("author_profile")
    if role == "verifier" and author:
        lift(author, "verifier_not_below_author")
    return PROFILES[level], rules


def effort_value_index(level, n):
    """Индекс значения клиента (по возрастанию, всего n) для уровня; см. model-selection."""
    if level == "low":
        return 0
    if level == "max":
        return n - 1
    if level == "high":
        return n - 2 if n >= 4 else n - 1
    return (n - 1) // 2


def decide_effort(profile, facts, judgments, rules):
    """Уровень усилия (уровень рассуждений) внутри выбранного профиля."""
    risky = any(r.startswith(("risk_signal:", "risk_unclear:", "review_md_")) for r in rules)
    large = judgments["size"]["p_large"] >= LARGE
    open_ = any(judgments[name]["verdict"] != "no" for name in OPEN_QUESTIONS)
    if profile == "light":
        return "low"
    if profile == "top":
        p_t3 = judgments["size"].get("p_t3", 0.0)
        return "max" if risky and p_t3 >= 0.5 else "high"
    # Неоднозначность поднимает усилие только решающим ролям: исполнитель задаёт вопрос.
    if large or risky or facts["role"] in ("verifier", "integrator") or (
            open_ and facts["role"] in DECIDING_ROLES):
        return "high"
    return "medium"


def top_policy(state):
    value = state.get("top_policy", "table")
    if value not in TOP_POLICIES:
        raise Fail(2, "bad_input", "field 'top_policy' must be one of " + ", ".join(TOP_POLICIES))
    return value


def calibrate(args):
    state = read_state(args.state)
    policy = top_policy(state)
    facts = {
        "role": require(state, "role", str, ROLES),
        "writes_code": require(state, "writes_code", bool),
        "review_md_risk": require(state, "review_md_risk", str, REVIEW_RISKS),
        "author_profile": state.get("author_profile"),
        # read_only: ни файлов, ни данных, ни внешних эффектов. Не указано — считается записью.
        "read_only": state.get("read_only", state.get("role") == "scout"),
    }
    if not isinstance(facts["read_only"], bool):
        raise Fail(2, "bad_input", "field 'read_only' must be true or false")
    if facts["read_only"] and facts["writes_code"]:
        raise Fail(2, "bad_input", "read_only and writes_code cannot both be true")
    if facts["author_profile"] is not None and facts["author_profile"] not in PROFILES:
        raise Fail(2, "bad_input", "field 'author_profile' must be one of " + ", ".join(PROFILES))
    task = {"goal": require(state, "goal", str),
            "acceptance_criteria": string_list(state, "acceptance_criteria"),
            "planned_paths": string_list(state, "planned_paths")}
    if facts["role"] == "scout":
        # Scout только собирает факты со ссылками: всегда light, Jev не нужен.
        if facts["writes_code"] or not facts["read_only"]:
            raise Fail(2, "bad_input", "scout is read-only; use role worker or dev-lead")
        return {"profile": "light", "table_profile": "light", "effort": "low",
                "rules": ["scout_facts_only"],
                "judgments": None, "model": None, "usage": None}
    jev_state = {"task": task}
    check_size(jev_state)
    config = offline_config() if args.response_file else load_config(args.project)
    result = system_one(config, jev_state, CALIBRATE_QUESTIONS, args.response_file)
    answers = result["answers"]
    size = answers["size"]
    probs = size.get("probabilities") if isinstance(size, dict) else None
    if not isinstance(probs, dict):
        raise Fail(4, "bad_response", "size answer has no probabilities")
    levels = [str(i) for i in range(len(CALIBRATE_QUESTIONS["size"]["criteria"]))]
    if set(probs) != set(levels):
        raise Fail(4, "bad_response", "size probabilities must cover levels " + ", ".join(levels))
    p_large = sum(prob(probs, k) for k in ("2", "3"))
    judgments = {"size": {"score": size.get("score"), "p_large": round(p_large, 4),
                          "p_t3": round(prob(probs, "3"), 4)}}
    for name in CALIBRATE_QUESTIONS:
        if name == "size":
            continue
        p = prob(answers[name], "noul")
        judgments[name] = {"p": p, "verdict": verdict(p)}
    table_profile, rules = decide_profile(facts, judgments)
    profile = table_profile
    # escalation_only: старшая модель не назначается сразу — только эскалацией после неудачи.
    # Verifier результата уже эскалированного автора остаётся не слабее автора.
    if (policy == "escalation_only" and profile == "top"
            and not (facts["role"] == "verifier" and facts["author_profile"] == "top")):
        profile = "coding"
        rules.append("top_by_escalation_only")
    effort = decide_effort(profile, facts, judgments, rules)
    return {"profile": profile, "table_profile": table_profile, "effort": effort,
            "rules": rules, "judgments": judgments,
            "model": result.get("model"), "usage": result.get("usage")}


def repair_cause(args):
    state = read_state(args.state)
    failed = state.get("failed_repairs")
    if isinstance(failed, bool) or not isinstance(failed, int) or failed < 0:
        raise Fail(2, "bad_input", "field 'failed_repairs' must be a non-negative integer")
    current = require(state, "current_profile", str, PROFILES)
    policy = top_policy(state)
    high_risk = state.get("high_risk", False)
    if not isinstance(high_risk, bool):
        raise Fail(2, "bad_input", "field 'high_risk' must be true or false")
    effort_now, n_values = state.get("current_effort"), state.get("effort_value_count")
    if effort_now is not None and effort_now not in EFFORTS:
        raise Fail(2, "bad_input", "field 'current_effort' must be one of " + ", ".join(EFFORTS))
    if n_values is not None and (isinstance(n_values, bool) or not isinstance(n_values, int)
                                 or n_values < 1):
        raise Fail(2, "bad_input", "field 'effort_value_count' must be a positive integer")
    next_effort = None
    if effort_now is not None and n_values is not None and current != "light":
        next_effort = next((level for level in EFFORTS[EFFORTS.index(effort_now) + 1:]
                            if effort_value_index(level, n_values)
                            > effort_value_index(effort_now, n_values)), None)
    jev_state = {"failure_report": require(state, "failure_report", str)}
    if state.get("check_output"):
        jev_state["check_output"] = str(state["check_output"])
    check_size(jev_state)
    config = offline_config() if args.response_file else load_config(args.project)
    result = system_one(config, jev_state, REPAIR_QUESTIONS, args.response_file)
    answer = result["answers"]["cause"]
    probs = answer.get("probabilities") if isinstance(answer, dict) else None
    if not isinstance(probs, dict) or not set(probs) <= set(CAUSES) or not probs:
        raise Fail(4, "bad_response", "cause answer has no valid probabilities")
    cause = max(probs, key=lambda k: prob(probs, k))
    p = prob(probs, cause)
    target_effort = None
    reasoning = cause == "reasoning_error" and p >= CAUSE_SURE
    if cause in BLOCKER_CAUSES and p >= CAUSE_SURE:
        action, target, rule = "blocker", None, "environment_or_access_is_not_model_problem"
    elif reasoning and failed == 1 and next_effort:
        # Сначала дешевле: то же назначение на той же модели с большим усилием.
        action, target, rule = "raise_effort", None, "reasoning_error_raise_effort_first"
        target_effort = next_effort
    elif (reasoning and failed >= 2 and current == "coding" and policy == "escalation_only"
          and not high_risk):
        action, target, rule = "parent_decides", None, "top_reserved_for_high_risk"
    elif reasoning and failed >= 2 and current != "top":
        action, target, rule = "escalate", PROFILES[PROFILES.index(current) + 1], \
            "reasoning_error_after_two_repairs"
    else:
        action, target, rule = "parent_decides", None, "not_settled_by_table"
    return {"action": action, "target_profile": target, "target_effort": target_effort,
            "rule": rule,
            "judgments": {"cause": cause, "p": p, "probabilities": probs},
            "model": result.get("model"), "usage": result.get("usage")}


def offline_config():
    return {"provider": "offline", "key": "", "model": DEFAULT_MODEL}


def check(args):
    config = load_config(args.project)
    result = call(config, "GET", "/v1/models")
    items = result.get("data", result.get("models")) if isinstance(result, dict) else result
    if not isinstance(items, list):
        raise Fail(4, "bad_response", "models list not found in response")
    names = [m.get("name") or m.get("id") if isinstance(m, dict) else m for m in items]
    out = public(config)
    out["models"] = names
    return out


def main(argv=None):
    parser = argparse.ArgumentParser(description="Jev judgments for orchestrate")
    parser.add_argument("--project", help="project root with .env (default: main git checkout)")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("config")
    sub.add_parser("check")
    for name in ("calibrate", "repair-cause"):
        p = sub.add_parser(name)
        p.add_argument("--state", required=True, help="state JSON file or - for stdin")
        p.add_argument("--response-file", help="saved API response instead of network (tests)")
    args = parser.parse_args(argv)
    try:
        if args.command == "config":
            out = public(load_config(args.project))
        elif args.command == "check":
            out = check(args)
        elif args.command == "calibrate":
            out = calibrate(args)
        else:
            out = repair_cause(args)
    except Fail as exc:
        print(json.dumps(dict({"error": exc.error, "message": str(exc)}, **exc.extra),
                         ensure_ascii=False))
        return exc.code
    print(json.dumps(out, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
