"""cccp - mirror Hermes sessions as Claude-Code-shaped jsonl and drive the cccp sync daemon.

Every hook is fail-open: exceptions are logged, never raised into the turn.
Historical sessions are backfilled from state.db (read-only) into the same transcripts.
"""
from __future__ import annotations

import argparse
import functools
import hashlib
import json
import logging
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import threading
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Tuple

logger = logging.getLogger(__name__)

AGENT = "hermes"
TRANSCRIPTS_DIR = "transcripts"
HOOK_TIMEOUT_SECONDS = 10
MAX_BLOCK_CHARS = 512 * 1024
MAX_SESSIONS = 256

CCCP_DIR = "cccp"
BACKFILL_STAMP = "backfill.json"
BACKFILL_LOCK = "backfill.lock"
LEGACY_STAMP = TRANSCRIPTS_DIR + "/.backfill.json"
BACKFILL_DELAY_SECONDS = 5.0
BACKFILL_BUDGET_SECONDS = 20.0
BACKFILL_ACTIVE_GRACE_SECONDS = 15 * 60
BACKFILL_UUID_NS = uuid.UUID("6b1c0c1e-6f2a-4a4b-9a2e-0ccb00000001")

_SAFE_ID = re.compile(r"[^A-Za-z0-9._-]")
_META_TEXT = re.compile(r"^\s*(<[a-z][a-z0-9_-]*>|\[(System|Cron|Scheduled|Context|Automated|User attached|The user attached)\b)")
TOOL_NAMES = {
    "terminal": "Bash",
    "read_file": "Read",
    "write_file": "Write",
    "patch": "Edit",
    "search_files": "Grep",
    "web_search": "WebSearch",
    "web_extract": "WebFetch",
    "delegate_task": "Agent",
    "todo_list": "TodoWrite",
    "skill_view": "Skill",
    "clarify": "AskUserQuestion",
}
_lock = threading.Lock()
_sessions: Dict[str, "_Session"] = {}
_backfill_state = {"started": False, "running": False, "last": None}


def _hermes_home() -> Path:
    try:
        from hermes_constants import get_hermes_home
        return Path(get_hermes_home())
    except Exception:
        return Path(os.environ.get("HERMES_HOME") or Path.home() / ".hermes")


def _iso(ts: Optional[float] = None) -> str:
    dt = datetime.now(timezone.utc) if ts is None else datetime.fromtimestamp(float(ts), timezone.utc)
    return dt.strftime("%Y-%m-%dT%H:%M:%S.") + f"{dt.microsecond // 1000:03d}Z"


def _now() -> str:
    return _iso()


def _text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        s = value
    elif isinstance(value, list) and all(isinstance(p, dict) for p in value):
        parts = [p.get("text") for p in value if p.get("type") == "text" and isinstance(p.get("text"), str)]
        s = "\n".join(parts) if parts else json.dumps(value, ensure_ascii=False, default=str)
    else:
        try:
            s = json.dumps(value, ensure_ascii=False, default=str)
        except Exception:
            s = str(value)
    if len(s) > MAX_BLOCK_CHARS:
        s = s[:MAX_BLOCK_CHARS] + f"\n... [cccp: truncated {len(s) - MAX_BLOCK_CHARS} chars]"
    return s


def _fingerprint(*parts: Any) -> str:
    h = hashlib.sha256()
    for p in parts:
        h.update(_text(p).encode("utf-8", "replace"))
        h.update(b"\x00")
    return h.hexdigest()


def _user_turns(history: Any) -> int:
    if not isinstance(history, list):
        return 0
    return sum(1 for m in history if isinstance(m, dict) and m.get("role") == "user")


def _last_reasoning(history: Any) -> str:
    if not isinstance(history, list):
        return ""
    for m in reversed(history):
        if isinstance(m, dict) and m.get("role") == "assistant":
            r = m.get("reasoning")
            return r if isinstance(r, str) else ""
    return ""


def _is_meta(text: str) -> bool:
    return bool(_META_TEXT.match(text))


def _claude_tool(name: Any, args: Any) -> Tuple[str, Dict[str, Any]]:
    name = str(name or "")
    tool_input: Dict[str, Any] = dict(args) if isinstance(args, dict) else {"args": args}
    claude = TOOL_NAMES.get(name)
    if name == "search_files" and tool_input.get("target") == "files":
        claude = "Glob"
    if claude is None:
        return name, tool_input
    if "path" in tool_input and "file_path" not in tool_input and name != "search_files":
        tool_input["file_path"] = tool_input["path"]
    if name == "web_extract" and "url" not in tool_input:
        urls = tool_input.get("urls")
        if isinstance(urls, list) and urls:
            tool_input["url"] = str(urls[0])
        elif isinstance(urls, str):
            tool_input["url"] = urls
    if name == "skill_view" and "name" in tool_input and "skill" not in tool_input:
        tool_input["skill"] = tool_input["name"]
    if name == "delegate_task":
        tasks = tool_input.get("tasks")
        first = tasks[0] if isinstance(tasks, list) and tasks and isinstance(tasks[0], dict) else {}
        goal = str(first.get("goal") or "")
        if goal and "prompt" not in tool_input:
            tool_input["prompt"] = goal if not first.get("context") else f"{goal}\n\n{first['context']}"
        if "description" not in tool_input:
            tool_input["description"] = (goal[:117] + "...") if len(goal) > 120 else goal
        if first.get("role") and "subagent_type" not in tool_input:
            tool_input["subagent_type"] = str(first["role"])
    tool_input["hermes_tool"] = name
    return claude, tool_input


def _usage_block(u: Dict[str, Any]) -> Dict[str, int]:
    def num(*keys: str) -> int:
        for k in keys:
            v = u.get(k)
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                return int(v)
        return 0
    return {
        "input_tokens": num("input_tokens", "prompt_tokens"),
        "output_tokens": num("output_tokens", "completion_tokens"),
        "cache_read_input_tokens": num("cache_read_tokens", "cache_read_input_tokens"),
        "cache_creation_input_tokens": num("cache_write_tokens", "cache_creation_input_tokens"),
        "reasoning_tokens": num("reasoning_tokens"),
    }


def _transcript_path(home: Path, session_id: str) -> Path:
    safe = _SAFE_ID.sub("_", session_id) or "unknown"
    return home / TRANSCRIPTS_DIR / f"{safe}.jsonl"


def _line(kind: str, message: Optional[Dict[str, Any]], *, session_id: str, cwd: str, parent: Optional[str],
          line_uuid: Optional[str] = None, timestamp: Optional[str] = None,
          extra: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    line: Dict[str, Any] = {
        "parentUuid": parent,
        "isSidechain": False,
        "userType": "external",
        "cwd": cwd,
        "sessionId": session_id,
        "version": AGENT,
        "type": kind,
        "uuid": line_uuid or str(uuid.uuid4()),
        "timestamp": timestamp or _now(),
    }
    if extra:
        line.update(extra)
    if message is not None:
        line["message"] = message
    return line


def _git_info(cwd: str) -> Dict[str, str]:
    out: Dict[str, str] = {}
    git = shutil.which("git")
    if not git or not cwd or not os.path.isdir(cwd):
        return out
    for key, args in (("gitBranch", ["rev-parse", "--abbrev-ref", "HEAD"]), ("gitCommit", ["rev-parse", "HEAD"])):
        try:
            r = subprocess.run([git, "-C", cwd, *args], capture_output=True, text=True, timeout=2)
        except Exception:
            return out
        value = (r.stdout or "").strip()
        if r.returncode != 0 or not value:
            return out
        if value != "HEAD":
            out[key] = value
    return out


def _user() -> str:
    try:
        import getpass
        return getpass.getuser()
    except Exception:
        return os.environ.get("USER", "")


def _meta_extra(cwd: str, model: str, source: str = "", parent: str = "", git: bool = True) -> Dict[str, Any]:
    extra: Dict[str, Any] = {"agentSource": source or "", "model": model or ""}
    if parent:
        extra["parentSessionId"] = parent
    user = _user()
    if user:
        extra["user"] = user
    if git:
        extra.update(_git_info(cwd))
    return extra


def _write_lines(path: Path, lines: Iterable[Dict[str, Any]]) -> int:
    body = [json.dumps(line, ensure_ascii=False, default=str) for line in lines]
    if not body:
        return 0
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "a", encoding="utf-8") as fh:
        fh.write("\n".join(body) + "\n")
    return len(body)


class _Session:
    def __init__(self, session_id: str, model: str = ""):
        self.id = session_id
        self.model = model or ""
        self.cwd = os.getcwd()
        self.last_uuid: Optional[str] = None
        self.seen: set = set()
        self.started = False
        self.meta_written = False
        self.source = ""
        self.parent = ""
        self.lock = threading.Lock()
        self.path = _transcript_path(_hermes_home(), session_id)

    def append(self, key: str, records: List[Tuple[Any, ...]], timestamp: Optional[str] = None) -> bool:
        with self.lock:
            if key in self.seen:
                return False
            self.seen.add(key)
            self._write(records, timestamp)
            return True

    def _write(self, records: List[Tuple[Any, ...]], timestamp: Optional[str] = None) -> None:
        lines = []
        if not self.meta_written:
            self.meta_written = True
            records = [("session-meta", None, _meta_extra(self.cwd, self.model, self.source, self.parent))] + list(records)
        for rec in records:
            kind, message = rec[0], rec[1]
            extra = rec[2] if len(rec) > 2 else None
            line = _line(kind, message, session_id=self.id, cwd=self.cwd, parent=self.last_uuid,
                         timestamp=timestamp, extra=extra)
            self.last_uuid = line["uuid"]
            lines.append(line)
        _write_lines(self.path, lines)

    def set_model(self, model: str) -> None:
        with self.lock:
            if not model or model == self.model:
                return
            self.model = model
            self._write([("model-change", None, {"model": model, "effort": ""})])


def _cccp(sub: str, session_id: str, background: bool = False) -> None:
    exe = shutil.which("cccp")
    if not exe:
        logger.debug("cccp plugin: cccp not on PATH, skipping watch %s", sub)
        return

    def run() -> None:
        try:
            env = dict(os.environ)
            env.setdefault("HERMES_HOME", str(_hermes_home()))
            r = subprocess.run(
                [exe, "watch", sub, "--agent", AGENT],
                input=json.dumps({"session_id": session_id}),
                capture_output=True, text=True, timeout=HOOK_TIMEOUT_SECONDS, env=env,
            )
            if r.returncode != 0:
                logger.warning("cccp watch %s exited %d: %s", sub, r.returncode, (r.stderr or "").strip()[:400])
        except Exception as exc:
            logger.warning("cccp watch %s failed: %s", sub, exc)

    if background:
        threading.Thread(target=run, name="cccp-hook", daemon=True).start()
    else:
        run()


def _session(session_id: Any, model: str = "", source: Any = None, parent: Any = None) -> Optional[_Session]:
    sid = str(session_id or "").strip()
    if not sid:
        return None
    with _lock:
        s = _sessions.get(sid)
        if s is None:
            if len(_sessions) >= MAX_SESSIONS:
                _sessions.pop(next(iter(_sessions)))
            s = _Session(sid)
            _sessions[sid] = s
        if source and not s.source:
            s.source = str(source)
        if parent and not s.parent and str(parent) != sid:
            s.parent = str(parent)
        start = not s.started
        s.started = True
    if start:
        _cccp("hook-start", sid, background=True)
    s.set_model(model or "")
    return s


def _failsafe(fn):
    @functools.wraps(fn)
    def wrapper(*args, **kwargs):
        try:
            fn(*args, **kwargs)
        except Exception as exc:
            logger.warning("cccp plugin %s failed: %s", fn.__name__, exc)
        return None
    return wrapper


@_failsafe
def _on_session_start(session_id=None, model="", platform=None, source=None, parent_session_id=None, **_):
    _session(session_id, model or "", source=platform or source, parent=parent_session_id)


@_failsafe
def _pre_llm_call(session_id=None, user_message=None, conversation_history=None, model="", **_):
    s = _session(session_id, model or "")
    text = _text(user_message)
    if s is None or not text.strip():
        return
    key = _fingerprint("user", text, _user_turns(conversation_history))
    extra = {"isMeta": True} if _is_meta(text) else None
    s.append(key, [("user", {"role": "user", "content": text}, extra)])


@_failsafe
def _post_tool_call(tool_name="", args=None, result=None, session_id=None, tool_call_id=None,
                    status=None, error_type=None, error_message=None, **_):
    s = _session(session_id)
    if s is None:
        return
    tid = str(tool_call_id or "").strip()
    key = "tool:" + (tid or _fingerprint(tool_name, args, result))
    if not tid:
        tid = "toolu_" + key[5:29]
    is_error = bool(error_type) or str(status or "").lower() in {"error", "failed", "cancelled", "timeout"}
    content = _text(result) if result is not None else _text(error_message)
    name, tool_input = _claude_tool(tool_name, args)
    s.append(key, [
        ("assistant", {"role": "assistant", "model": s.model,
                       "content": [{"type": "tool_use", "id": tid, "name": name, "input": tool_input}]}),
        ("user", {"role": "user",
                  "content": [{"type": "tool_result", "tool_use_id": tid, "content": content, "is_error": is_error}]}),
    ])


@_failsafe
def _post_llm_call(session_id=None, assistant_response=None, conversation_history=None, model="", **_):
    s = _session(session_id, model or "")
    text = _text(assistant_response)
    if s is None or not text.strip():
        return
    key = _fingerprint("assistant", text, _user_turns(conversation_history))
    content: List[Dict[str, Any]] = []
    reasoning = _last_reasoning(conversation_history)
    if reasoning.strip():
        content.append({"type": "thinking", "thinking": _text(reasoning)})
    content.append({"type": "text", "text": text})
    s.append(key, [("assistant", {"role": "assistant", "model": s.model or model or "", "content": content})])


@_failsafe
def _post_api_request(session_id=None, model="", usage=None, ended_at=None, api_request_id=None, **_):
    s = _session(session_id, model or "")
    if s is None or not isinstance(usage, dict):
        return
    key = "usage:" + (str(api_request_id) if api_request_id else _fingerprint(model, ended_at, usage))
    ts = _iso(ended_at) if isinstance(ended_at, (int, float)) and ended_at > 0 else None
    s.append(key, [("usage", None, {"model": s.model or model or "", "usage": _usage_block(usage)})], timestamp=ts)


@_failsafe
def _on_session_finalize(session_id=None, **_):
    sid = str(session_id or "").strip()
    with _lock:
        s = _sessions.pop(sid, None)
    if s is not None and s.started:
        _cccp("hook-end", sid)


_CONTENT_JSON_PREFIX = "\x00json:"
_SESSION_COLS = ("id", "started_at", "ended_at", "cwd", "model", "message_count", "input_tokens", "output_tokens",
                 "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "estimated_cost_usd", "actual_cost_usd",
                 "source", "parent_session_id")
_MESSAGE_COLS = ("id", "role", "content", "tool_call_id", "tool_calls", "tool_name", "timestamp",
                 "reasoning", "reasoning_content", "active", "compacted")


def _json_or(raw: Any, default: Any) -> Any:
    if not isinstance(raw, str):
        return raw if raw is not None else default
    try:
        return json.loads(raw)
    except Exception:
        return default


def _decode_content(content: Any) -> Any:
    if isinstance(content, str) and content.startswith(_CONTENT_JSON_PREFIX):
        return _json_or(content[len(_CONTENT_JSON_PREFIX):], content)
    return content


class _Store:
    def __init__(self, home: Path, use_api: bool = True):
        self.db_path = home / "state.db"
        if not self.db_path.is_file():
            raise FileNotFoundError(str(self.db_path))
        self.api = None
        self.conn: Optional[sqlite3.Connection] = None
        if use_api:
            try:
                from hermes_state import SessionDB
                self.api = SessionDB(db_path=self.db_path, read_only=True)
            except Exception as exc:
                logger.debug("cccp backfill: hermes_state unavailable (%s), using read-only sql", exc)
        if self.api is None:
            self.conn = sqlite3.connect(f"file:{self.db_path}?mode=ro", uri=True, timeout=1.0,
                                        check_same_thread=False)
            self.conn.row_factory = sqlite3.Row

    def close(self) -> None:
        try:
            if self.api is not None:
                self.api.close()
            if self.conn is not None:
                self.conn.close()
        except Exception:
            pass

    def sessions(self) -> List[Dict[str, Any]]:
        rows: List[Dict[str, Any]] = []
        if self.api is not None:
            try:
                rows = self.api.list_sessions_rich(limit=1_000_000, include_children=True, include_archived=True,
                                                   include_hidden=True, compact_rows=True)
            except Exception as exc:
                logger.debug("cccp backfill: list_sessions_rich failed (%s), using sql", exc)
                rows = []
        if not rows:
            rows = [dict(r) for r in self._sql("SELECT * FROM sessions")]
        out = [{k: r.get(k) for k in _SESSION_COLS} for r in rows if r.get("id")]
        out.sort(key=lambda r: (r.get("started_at") or 0, r["id"]))
        return out

    def usage_rows(self, session_id: str) -> List[Dict[str, Any]]:
        try:
            rows = [dict(r) for r in self._sql("SELECT * FROM session_model_usage WHERE session_id = ?", (session_id,))]
        except sqlite3.Error:
            return []
        rows.sort(key=lambda r: (str(r.get("task") or ""), str(r.get("model") or ""), r.get("first_seen") or 0))
        return rows

    def last_message_id(self, session_id: str) -> int:
        if self.api is not None:
            try:
                rows = self.api.get_messages(session_id, latest=True, limit=1)
                return int(rows[-1]["id"]) if rows else 0
            except Exception as exc:
                logger.debug("cccp backfill: get_messages(latest) failed (%s), using sql", exc)
        row = self._sql("SELECT MAX(id) AS m FROM messages WHERE session_id = ?" + self._live_filter(" AND "),
                        (session_id,))
        return int(row[0]["m"] or 0) if row else 0

    def _message_columns(self) -> set:
        if not hasattr(self, "_msg_cols"):
            self._msg_cols = {r["name"] for r in self._sql("PRAGMA table_info(messages)")}
        return self._msg_cols

    def _live_filter(self, prefix: str) -> str:
        cols = self._message_columns()
        if "active" in cols and "compacted" in cols:
            return prefix + "(active = 1 OR compacted = 1)"
        if "active" in cols:
            return prefix + "active = 1"
        return ""

    def messages(self, session_id: str) -> List[Dict[str, Any]]:
        if self.api is not None:
            try:
                return [dict(m) for m in self.api.get_messages(session_id, include_compacted=True)]
            except Exception as exc:
                logger.debug("cccp backfill: get_messages failed (%s), using sql", exc)
        rows = self._sql("SELECT * FROM messages WHERE session_id = ?" + self._live_filter(" AND ")
                         + " ORDER BY id ASC", (session_id,))
        cols = self._message_columns()
        has_active = "active" in cols

        def col(r: sqlite3.Row, name: str) -> Any:
            return r[name] if name in cols else None

        seen: Dict[Tuple[Any, ...], sqlite3.Row] = {}
        for r in rows:
            key = (r["role"], r["content"], r["timestamp"], col(r, "tool_call_id"), col(r, "tool_calls"),
                   col(r, "tool_name"))
            rank = (r["active"] if has_active else 1, r["id"])
            cur = seen.get(key)
            if cur is None or rank > ((cur["active"] if has_active else 1), cur["id"]):
                seen[key] = r
        out = []
        for r in sorted(seen.values(), key=lambda r: r["id"]):
            m = {k: r[k] for k in r.keys()}
            m["content"] = _decode_content(m.get("content"))
            if m.get("tool_calls"):
                m["tool_calls"] = _json_or(m["tool_calls"], [])
            out.append(m)
        return out

    def _sql(self, sql: str, params: Tuple[Any, ...] = ()) -> List[sqlite3.Row]:
        conn = self.conn
        if conn is None:
            conn = self.conn = sqlite3.connect(f"file:{self.db_path}?mode=ro", uri=True, timeout=1.0,
                                               check_same_thread=False)
            conn.row_factory = sqlite3.Row
        return conn.execute(sql, params).fetchall()


def _det_uuid(session_id: str, message_id: Any, tag: str) -> str:
    return str(uuid.uuid5(BACKFILL_UUID_NS, f"{session_id}\x00{message_id}\x00{tag}"))


def _tool_calls_of(msg: Dict[str, Any]) -> List[Dict[str, Any]]:
    calls = msg.get("tool_calls")
    if isinstance(calls, str):
        calls = _json_or(calls, [])
    return [c for c in calls if isinstance(c, dict)] if isinstance(calls, list) else []


def _result_is_error(decoded: Any) -> bool:
    if isinstance(decoded, dict):
        if decoded.get("error"):
            return True
        if decoded.get("success") is False:
            return True
    return False


def _usage_of(row: Dict[str, Any]) -> Tuple[Dict[str, int], float]:
    block = _usage_block({
        "input_tokens": row.get("input_tokens"), "output_tokens": row.get("output_tokens"),
        "cache_read_tokens": row.get("cache_read_tokens"), "cache_write_tokens": row.get("cache_write_tokens"),
        "reasoning_tokens": row.get("reasoning_tokens"),
    })
    cost = row.get("actual_cost_usd") or row.get("estimated_cost_usd") or 0
    return block, float(cost) if isinstance(cost, (int, float)) else 0.0


def _backfill_lines(session: Dict[str, Any], msgs: List[Dict[str, Any]],
                    usage_rows: Optional[List[Dict[str, Any]]] = None) -> List[Dict[str, Any]]:
    sid = str(session["id"])
    cwd = str(session.get("cwd") or "")
    model = str(session.get("model") or "")
    results: Dict[str, List[Dict[str, Any]]] = {}
    for m in msgs:
        if m.get("role") == "tool" and m.get("tool_call_id"):
            results.setdefault(str(m["tool_call_id"]), []).append(m)
    consumed: set = set()
    out: List[Dict[str, Any]] = []
    parent: Optional[str] = None

    def emit(kind: str, message: Optional[Dict[str, Any]], mid: Any, tag: str, ts: Any,
             extra: Optional[Dict[str, Any]] = None) -> None:
        nonlocal parent
        line = _line(kind, message, session_id=sid, cwd=cwd, parent=parent,
                     line_uuid=_det_uuid(sid, mid, tag), timestamp=_iso(ts or 0), extra=extra)
        parent = line["uuid"]
        out.append(line)

    stamps = [float(t) for t in (session.get("started_at"), msgs[0].get("timestamp") if msgs else None) if t]
    first_ts = min(stamps) if stamps else None
    emit("session-meta", None, "", "meta", first_ts,
         _meta_extra(cwd, model, str(session.get("source") or ""), str(session.get("parent_session_id") or ""), git=False))
    if model:
        emit("model-change", None, "", "model", first_ts, {"model": model, "effort": ""})

    def emit_result(tool_row: Dict[str, Any], tid: str) -> None:
        decoded = _decode_content(tool_row.get("content"))
        emit("user", {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tid,
                                                   "content": _text(decoded), "is_error": _result_is_error(decoded)}]},
             tool_row["id"], "result", tool_row.get("timestamp"))

    for m in msgs:
        role, mid, ts = m.get("role"), m.get("id"), m.get("timestamp")
        if role == "user":
            text = _text(_decode_content(m.get("content")))
            if text.strip():
                emit("user", {"role": "user", "content": text}, mid, "text", ts, {"isMeta": True} if _is_meta(text) else None)
        elif role == "assistant":
            text = _text(_decode_content(m.get("content")))
            reasoning = m.get("reasoning") or m.get("reasoning_content") or ""
            thinking = [{"type": "thinking", "thinking": _text(reasoning)}] if str(reasoning).strip() else []
            calls = _tool_calls_of(m)
            if text.strip():
                emit("assistant", {"role": "assistant", "model": model, "content": thinking + [{"type": "text", "text": text}]},
                     mid, "text", ts)
                thinking = []
            for i, call in enumerate(calls):
                fn = call.get("function") if isinstance(call.get("function"), dict) else {}
                tid = str(call.get("id") or call.get("call_id") or f"toolu_{_det_uuid(sid, mid, f'call{i}')[:24]}")
                args = fn.get("arguments", call.get("arguments"))
                if isinstance(args, str):
                    args = _json_or(args, {"args": args})
                name, tool_input = _claude_tool(fn.get("name") or call.get("name"), args)
                emit("assistant", {"role": "assistant", "model": model,
                                   "content": thinking + [{"type": "tool_use", "id": tid, "name": name, "input": tool_input}]},
                     mid, f"call/{tid}", ts)
                thinking = []
                pending = [r for r in results.get(tid, []) if r["id"] not in consumed and r["id"] > mid]
                if pending:
                    consumed.add(pending[0]["id"])
                    emit_result(pending[0], tid)
            if thinking:
                emit("assistant", {"role": "assistant", "model": model, "content": thinking}, mid, "thinking", ts)
        elif role == "tool":
            if mid in consumed:
                continue
            consumed.add(mid)
            emit_result(m, str(m.get("tool_call_id") or f"toolu_{_det_uuid(sid, mid, 'orphan')[:24]}"))
    end_ts = session.get("ended_at") or (msgs[-1].get("timestamp") if msgs else None)
    rows = [r for r in (usage_rows or []) if isinstance(r, dict)]
    if not rows and any(session.get(k) for k in ("input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens")):
        rows = [dict(session, task="")]
    for r in rows:
        block, cost = _usage_of(r)
        if not any(block.values()):
            continue
        tag = f"usage/{r.get('model') or model}/{r.get('task') or ''}"
        extra: Dict[str, Any] = {"model": str(r.get("model") or model), "usage": block}
        if cost:
            extra["costUSD"] = cost
        if r.get("task"):
            extra["task"] = str(r["task"])
        emit("usage", None, "", tag, end_ts, extra)
    return out


def _load_stamp(path: Path) -> Dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        if isinstance(data, dict) and isinstance(data.get("sessions"), dict):
            return data
    except Exception:
        pass
    return {"version": 1, "sessions": {}}


def _save_stamp(path: Path, stamp: Dict[str, Any]) -> None:
    stamp["version"] = 1
    stamp["updated"] = _now()
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(stamp, indent=1, sort_keys=True), encoding="utf-8")
    os.replace(tmp, path)


def _stamp_path(home: Path) -> Path:
    return home / CCCP_DIR / BACKFILL_STAMP


def _migrate_stamp(home: Path) -> None:
    new, old = _stamp_path(home), home / LEGACY_STAMP
    if new.exists() or not old.is_file():
        return
    stamp = _load_stamp(old)
    if stamp["sessions"]:
        _save_stamp(new, stamp)


def _read_stamp(home: Path) -> Dict[str, Any]:
    new = _stamp_path(home)
    return _load_stamp(new if new.exists() else home / LEGACY_STAMP)


def _file_uuids(path: Path) -> Tuple[set, Optional[str]]:
    uuids: set = set()
    last: Optional[str] = None
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        for raw in fh:
            raw = raw.strip()
            if not raw:
                continue
            try:
                u = json.loads(raw).get("uuid")
            except Exception:
                continue
            if isinstance(u, str):
                uuids.add(u)
                last = u
    return uuids, last


class _Lock:
    def __init__(self, path: Path):
        self.path = path
        self.fh = None

    def __enter__(self) -> bool:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.fh = open(self.path, "a+")
        try:
            import fcntl
            fcntl.flock(self.fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except ImportError:
            return True
        except OSError:
            self.fh.close()
            self.fh = None
            return False
        return True

    def __exit__(self, *exc) -> None:
        if self.fh is not None:
            self.fh.close()


def backfill(home: Optional[Path] = None, budget: float = BACKFILL_BUDGET_SECONDS, use_api: bool = True,
             now: Optional[float] = None) -> Dict[str, Any]:
    home = Path(home) if home is not None else _hermes_home()
    t0 = time.monotonic()
    stats: Dict[str, Any] = {"home": str(home), "sessions": 0, "written": 0, "lines": 0, "skipped": 0,
                             "active": 0, "empty": 0, "foreign": 0, "resume": False, "seconds": 0.0, "api": False}
    with _Lock(home / CCCP_DIR / BACKFILL_LOCK) as locked:
        if not locked:
            stats["skipped_locked"] = True
            return stats
        _migrate_stamp(home)
        stamp_path = _stamp_path(home)
        stamp = _load_stamp(stamp_path)
        entries: Dict[str, Any] = stamp["sessions"]
        store = _Store(home, use_api=use_api)
        stats["api"] = store.api is not None
        wall = time.time() if now is None else now
        try:
            with _lock:
                live = set(_sessions)
            sessions = store.sessions()
            stats["sessions"] = len(sessions)
            dirty = False
            for s in sessions:
                if time.monotonic() - t0 > budget:
                    stats["resume"] = True
                    break
                sid = str(s["id"])
                entry = entries.get(sid) or {}
                if sid in live or entry.get("skip"):
                    stats["skipped"] += 1
                    continue
                path = _transcript_path(home, sid)
                last_id = store.last_message_id(sid)
                if entry and path.is_file() and entry.get("last_id") == last_id and entry.get("size") == path.stat().st_size:
                    stats["skipped"] += 1
                    continue
                msgs = store.messages(sid)
                if not msgs:
                    stats["empty"] += 1
                    continue
                newest = max(float(m.get("timestamp") or 0) for m in msgs)
                if s.get("ended_at") is None and wall - newest < BACKFILL_ACTIVE_GRACE_SECONDS:
                    stats["active"] += 1
                    continue
                lines = _backfill_lines(s, msgs, store.usage_rows(sid))
                if path.is_file():
                    existing, parent = _file_uuids(path)
                    if existing and not any(l["uuid"] in existing for l in lines):
                        entries[sid] = {"skip": "live"}
                        stats["foreign"] += 1
                        dirty = True
                        continue
                    lines = [l for l in lines if l["uuid"] not in existing]
                    if existing and lines:
                        lines[0]["parentUuid"] = parent
                n = _write_lines(path, lines)
                entries[sid] = {"last_id": max(int(m["id"]) for m in msgs), "size": path.stat().st_size if path.is_file() else 0,
                                "ended_at": s.get("ended_at")}
                dirty = True
                if n:
                    stats["written"] += 1
                    stats["lines"] += n
                    _save_stamp(stamp_path, stamp)
                    dirty = False
            if dirty or not stamp_path.is_file():
                _save_stamp(stamp_path, stamp)
        finally:
            store.close()
    stats["seconds"] = round(time.monotonic() - t0, 3)
    return stats


def _backfill_safely(budget: float = BACKFILL_BUDGET_SECONDS) -> Optional[Dict[str, Any]]:
    if _backfill_state["running"]:
        return None
    _backfill_state["running"] = True
    try:
        stats = backfill(budget=budget)
        _backfill_state["last"] = stats
        if stats.get("skipped_locked"):
            logger.debug("cccp backfill: another process holds the lock, skipping")
        elif stats["written"] or stats["resume"]:
            logger.info("cccp backfill: %d/%d sessions written, %d lines in %.1fs%s", stats["written"],
                        stats["sessions"], stats["lines"], stats["seconds"],
                        " (budget hit, resuming next launch)" if stats["resume"] else "")
        else:
            logger.debug("cccp backfill: nothing to do (%s)", stats)
        return stats
    except Exception as exc:
        logger.warning("cccp backfill failed: %s", exc)
        return None
    finally:
        _backfill_state["running"] = False


def _start_backfill_thread() -> None:
    if _backfill_state["started"] or os.environ.get("CCCP_BACKFILL", "1").strip().lower() in {"0", "false", "no", "off"}:
        return
    _backfill_state["started"] = True
    t = threading.Timer(BACKFILL_DELAY_SECONDS, _backfill_safely)
    t.daemon = True
    t.name = "cccp-backfill"
    t.start()


_SLASH_HELP = ("cccp: backfill Hermes history into cccp transcripts\n"
               "  /cccp backfill [seconds]   backfill past sessions (default budget 20 s; 0 = unlimited)\n"
               "  /cccp status               stamp summary and last run")


def _slash(raw_args: str = "") -> Optional[str]:
    try:
        argv = (raw_args or "").strip().split()
        cmd = argv[0] if argv else "help"
        if cmd == "backfill":
            budget = float(argv[1]) if len(argv) > 1 else BACKFILL_BUDGET_SECONDS
            stats = _backfill_safely(budget=budget if budget > 0 else float("inf"))
            if stats is None:
                return "cccp backfill: already running or failed (see hermes log)"
            if stats.get("skipped_locked"):
                return "cccp backfill: another Hermes process is backfilling, try again later"
            return (f"cccp backfill: {stats['written']} sessions written ({stats['lines']} lines), "
                    f"{stats['skipped']} unchanged, {stats['active']} active, {stats['empty']} empty, "
                    f"{stats['foreign']} live-owned, {stats['seconds']}s"
                    + (" -- budget hit, run again to continue" if stats["resume"] else ""))
        if cmd == "status":
            stamp = _read_stamp(_hermes_home())
            done = [k for k, v in stamp["sessions"].items() if not v.get("skip")]
            skipped = len(stamp["sessions"]) - len(done)
            return (f"cccp backfill: {len(done)} sessions backfilled, {skipped} live-owned, "
                    f"last stamp {stamp.get('updated') or 'never'}, running={_backfill_state['running']}, "
                    f"last run {_backfill_state['last']}")
        return _SLASH_HELP
    except Exception as exc:
        return f"cccp: {exc}"


def register(ctx):
    ctx.register_hook("on_session_start", _on_session_start)
    ctx.register_hook("pre_llm_call", _pre_llm_call)
    ctx.register_hook("post_tool_call", _post_tool_call)
    ctx.register_hook("post_llm_call", _post_llm_call)
    ctx.register_hook("post_api_request", _post_api_request)
    ctx.register_hook("on_session_finalize", _on_session_finalize)
    try:
        ctx.register_command("cccp", handler=_slash, description="cccp sync: backfill past sessions, status",
                             args_hint="[backfill [seconds]|status]")
    except Exception as exc:
        logger.debug("cccp plugin: register_command unavailable: %s", exc)
    try:
        _start_backfill_thread()
    except Exception as exc:
        logger.warning("cccp backfill thread not started: %s", exc)


def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(prog="cccp-hermes", description="cccp Hermes plugin: backfill state.db into transcripts")
    p.add_argument("--backfill", nargs="?", const="", metavar="HERMES_HOME",
                   help="backfill HERMES_HOME (default: $HERMES_HOME or ~/.hermes)")
    p.add_argument("--budget", type=float, default=0, help="time budget in seconds (0 = unlimited)")
    p.add_argument("--no-api", action="store_true", help="read state.db with plain sqlite instead of hermes_state")
    p.add_argument("-v", "--verbose", action="store_true")
    a = p.parse_args(argv)
    logging.basicConfig(level=logging.DEBUG if a.verbose else logging.INFO, format="%(levelname)s %(message)s")
    if a.backfill is None:
        p.print_help()
        return 2
    home = Path(a.backfill) if a.backfill else _hermes_home()
    stats = backfill(home=home, budget=a.budget if a.budget > 0 else float("inf"), use_api=not a.no_api)
    print(json.dumps(stats, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
