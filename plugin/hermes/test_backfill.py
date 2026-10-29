import importlib.util
import json
import os
import sqlite3
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("cccp_hermes", HERE / "__init__.py")
plugin = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plugin)

try:
    from hermes_state import SessionDB
except Exception:
    SessionDB = None

T0 = 1_700_000_000.0
NOW = T0 + 3600.0
SCHEMA = """
CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, model TEXT, started_at REAL NOT NULL,
    ended_at REAL, cwd TEXT, message_count INTEGER DEFAULT 0, parent_session_id TEXT);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
    content TEXT, tool_call_id TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL NOT NULL, reasoning TEXT,
    reasoning_content TEXT, active INTEGER NOT NULL DEFAULT 1, compacted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE session_model_usage (session_id TEXT NOT NULL, model TEXT NOT NULL, billing_provider TEXT NOT NULL DEFAULT '',
    billing_base_url TEXT NOT NULL DEFAULT '', billing_mode TEXT NOT NULL DEFAULT '', task TEXT NOT NULL DEFAULT '',
    api_call_count INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0, estimated_cost_usd REAL NOT NULL DEFAULT 0, actual_cost_usd REAL NOT NULL DEFAULT 0,
    cost_status TEXT, cost_source TEXT, first_seen REAL NOT NULL DEFAULT 0, last_seen REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task));
"""
USAGE_COLS = ("session_id", "model", "billing_provider", "billing_base_url", "billing_mode", "task", "api_call_count",
              "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
              "estimated_cost_usd", "actual_cost_usd", "first_seen", "last_seen")
USAGE_ROWS = [
    ("s_ended", "m1", "", "", "", "", 2, 100, 20, 5, 0, 3, 0.01, 0.0, T0 + 2, T0 + 4),
    ("s_ended", "m1", "", "", "", "background_review", 1, 40, 8, 0, 0, 0, 0.002, 0.0, T0 + 5, T0 + 5),
    ("s_empty", "m1", "", "", "", "", 1, 0, 0, 0, 0, 0, 0.0, 0.0, T0 + 1, T0 + 1),
]

SESSIONS = [
    {"id": "s_ended", "cwd": "/home/dev/proj", "model": "m1", "started_at": T0, "ended_at": T0 + 100},
    {"id": "s_empty", "cwd": "/home/dev/x", "model": "m1", "started_at": T0 + 1, "ended_at": T0 + 2},
    {"id": "s_stale_open", "cwd": "/home/dev/y", "model": "m2", "started_at": T0 + 10, "ended_at": None},
    {"id": "s_live", "cwd": "/home/dev/z", "model": "m2", "started_at": T0 + 20, "ended_at": None},
]
CALL = [{"id": "call_1", "type": "function", "function": {"name": "terminal", "arguments": json.dumps({"command": "ls"})}}]
MESSAGES = {
    "s_ended": [
        dict(role="user", content="hello there", timestamp=T0 + 1),
        dict(role="assistant", content="", tool_calls=json.dumps(CALL), timestamp=T0 + 2, reasoning="think first"),
        dict(role="tool", content=json.dumps({"output": "a.go\n", "exit_code": 0, "error": None}),
             tool_call_id="call_1", tool_name="terminal", timestamp=T0 + 3),
        dict(role="assistant", content="one file", timestamp=T0 + 4),
    ],
    "s_stale_open": [
        dict(role="user", content="[System] Injected by the platform.", timestamp=T0 + 10.5),
        dict(role="user", content="stale but done", timestamp=T0 + 11),
        dict(role="assistant", content="ok", timestamp=T0 + 12),
    ],
    "s_live": [
        dict(role="user", content="still typing", timestamp=NOW - 5),
    ],
}


def insert_usage(conn: sqlite3.Connection) -> None:
    conn.executemany(f"INSERT INTO session_model_usage({', '.join(USAGE_COLS)}) VALUES ({', '.join('?' * len(USAGE_COLS))})",
                     USAGE_ROWS)


def build_db(home: Path, use_api: bool) -> None:
    home.mkdir(parents=True, exist_ok=True)
    path = home / "state.db"
    if use_api:
        db = SessionDB(db_path=path)
        try:
            for s in SESSIONS:
                db.create_session(s["id"], "cli", model=s["model"], cwd=s["cwd"])
                for m in MESSAGES.get(s["id"], []):
                    db.append_message(s["id"], **m)
                if s["ended_at"] is not None:
                    db.end_session(s["id"], "cli_close")
        finally:
            db.close()
        conn = sqlite3.connect(path)
        insert_usage(conn)
        conn.commit()
        conn.close()
        return
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    insert_usage(conn)
    for s in SESSIONS:
        conn.execute("INSERT INTO sessions(id, source, model, started_at, ended_at, cwd) VALUES (?,?,?,?,?,?)",
                     (s["id"], "cli", s["model"], s["started_at"], s["ended_at"], s["cwd"]))
        for m in MESSAGES.get(s["id"], []):
            conn.execute("INSERT INTO messages(session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, reasoning)"
                         " VALUES (?,?,?,?,?,?,?,?)",
                         (s["id"], m["role"], m.get("content"), m.get("tool_call_id"), m.get("tool_calls"),
                          m.get("tool_name"), m["timestamp"], m.get("reasoning")))
    conn.commit()
    conn.close()


def read_lines(path: Path):
    return [json.loads(l) for l in path.read_text().splitlines() if l.strip()]


class BackfillCase:
    use_api = False

    def setUp(self):
        import tempfile
        self.tmp = tempfile.TemporaryDirectory()
        self.home = Path(self.tmp.name) / "hermes"
        os.environ["HERMES_HOME"] = str(self.home)
        build_db(self.home, self.use_api)
        self.tdir = self.home / "transcripts"
        self.stamp = self.home / "cccp" / plugin.BACKFILL_STAMP
        self.legacy = self.home / "transcripts" / ".backfill.json"
        plugin._sessions.clear()
        self._cccp = plugin._cccp
        plugin._cccp = lambda *a, **k: None

    def tearDown(self):
        plugin._cccp = self._cccp
        self.tmp.cleanup()

    def run_backfill(self, **kw):
        kw.setdefault("now", NOW)
        kw.setdefault("use_api", self.use_api)
        return plugin.backfill(home=self.home, budget=float("inf"), **kw)

    def test_first_run_writes_ended_and_stale_sessions_only(self):
        st = self.run_backfill()
        self.assertEqual(st["api"], self.use_api)
        self.assertEqual((st["sessions"], st["written"], st["empty"], st["active"]), (4, 2, 1, 1))
        files = sorted(p.name for p in self.tdir.glob("*.jsonl"))
        self.assertEqual(files, ["s_ended.jsonl", "s_stale_open.jsonl"])
        self.assertTrue(self.stamp.is_file())
        self.assertEqual(sorted(p.name for p in self.tdir.iterdir() if not p.name.endswith(".jsonl")), [])

    def test_line_shape_matches_live_hooks(self):
        self.run_backfill()
        lines = read_lines(self.tdir / "s_ended.jsonl")
        meta = lines.pop(0)
        self.assertEqual((meta["type"], meta["agentSource"], meta["model"], meta["uuid"]),
                         ("session-meta", "cli", "m1", plugin._det_uuid("s_ended", "", "meta")))
        self.assertNotIn("parentSessionId", meta)
        self.assertNotIn("gitCommit", meta)
        self.assertIsNone(meta["parentUuid"])
        self.assertEqual(lines[0]["parentUuid"], meta["uuid"])
        lines[0]["parentUuid"] = None
        self.assertEqual([l["type"] for l in lines], ["model-change", "user", "assistant", "user", "assistant", "usage", "usage"])
        base = ["parentUuid", "isSidechain", "userType", "cwd", "sessionId", "version", "type", "uuid", "timestamp"]
        for l in lines:
            own = {"model-change": ["model", "effort"], "usage": ["model", "usage", "costUSD"]}.get(l["type"], ["message"])
            if l.get("task"):
                own.append("task")
            self.assertEqual(sorted(l), sorted(base + own))
            self.assertEqual((l["cwd"], l["sessionId"], l["version"], l["userType"], l["isSidechain"]),
                             ("/home/dev/proj", "s_ended", "hermes", "external", False))
        self.assertEqual(lines[0]["timestamp"], "2023-11-14T22:13:21.000Z" if self.use_api else "2023-11-14T22:13:20.000Z")
        self.assertEqual((lines[0]["model"], lines[0]["effort"]), ("m1", ""))
        self.assertEqual(lines[0]["uuid"], plugin._det_uuid("s_ended", "", "model"))
        self.assertEqual(lines[1]["timestamp"], "2023-11-14T22:13:21.000Z")
        self.assertIsNone(lines[0]["parentUuid"])
        self.assertEqual([l["parentUuid"] for l in lines[1:]], [l["uuid"] for l in lines[:-1]])
        self.assertEqual(lines[1]["message"], {"role": "user", "content": "hello there"})
        self.assertNotIn("isMeta", lines[1])
        tool_use = lines[2]["message"]
        self.assertEqual(tool_use["model"], "m1")
        self.assertEqual(tool_use["content"][0], {"type": "thinking", "thinking": "think first"})
        self.assertEqual(tool_use["content"][1], {"type": "tool_use", "id": "call_1", "name": "Bash",
                                                  "input": {"command": "ls", "hermes_tool": "terminal"}})
        result = lines[3]["message"]["content"][0]
        self.assertEqual((result["type"], result["tool_use_id"], result["is_error"]), ("tool_result", "call_1", False))
        self.assertIn("a.go", result["content"])
        self.assertEqual(lines[4]["message"]["content"], [{"type": "text", "text": "one file"}])
        self.assertEqual(lines[1]["uuid"], plugin._det_uuid("s_ended", 1, "text"))
        usage = lines[5]
        self.assertEqual((usage["model"], usage["costUSD"]), ("m1", 0.01))
        if self.use_api:
            self.assertGreaterEqual(usage["timestamp"], lines[4]["timestamp"])
        else:
            self.assertEqual(usage["timestamp"], "2023-11-14T22:15:00.000Z")
        self.assertEqual(usage["usage"], {"input_tokens": 100, "output_tokens": 20, "cache_read_input_tokens": 5,
                                          "cache_creation_input_tokens": 0, "reasoning_tokens": 3})
        self.assertEqual(usage["uuid"], plugin._det_uuid("s_ended", "", "usage/m1/"))
        self.assertEqual((lines[6]["task"], lines[6]["usage"]["input_tokens"]), ("background_review", 40))
        self.assertFalse((self.tdir / "s_empty.jsonl").exists())

    def test_injected_prompts_are_meta(self):
        self.run_backfill()
        users = [l for l in read_lines(self.tdir / "s_stale_open.jsonl") if l["type"] == "user"]
        self.assertEqual([u.get("isMeta") for u in users], [True, None])
        self.assertTrue(users[0]["message"]["content"].startswith("[System]"))

    def test_claude_tool_mapping(self):
        cases = {
            ("terminal", ("command",)): ("Bash", ("command",)),
            ("read_file", ("path", "limit")): ("Read", ("file_path", "path", "limit")),
            ("write_file", ("path", "content")): ("Write", ("file_path", "content")),
            ("patch", ("path", "old_string", "new_string")): ("Edit", ("file_path", "old_string", "new_string")),
            ("search_files", ("pattern", "path")): ("Grep", ("pattern", "path")),
            ("web_search", ("query",)): ("WebSearch", ("query",)),
            ("clarify", ("questions",)): ("AskUserQuestion", ("questions",)),
            ("todo_list", ("todos",)): ("TodoWrite", ("todos",)),
            ("skill_view", ("name",)): ("Skill", ("skill", "name")),
        }
        for (name, keys), (want, want_keys) in cases.items():
            got, inp = plugin._claude_tool(name, {k: "v" for k in keys})
            self.assertEqual(got, want, name)
            self.assertEqual(inp["hermes_tool"], name)
            for k in want_keys:
                self.assertIn(k, inp, (name, k))
        self.assertEqual(plugin._claude_tool("search_files", {"pattern": "*.py", "target": "files"})[0], "Glob")
        self.assertEqual(plugin._claude_tool("web_extract", {"urls": ["http://a", "http://b"]})[1]["url"], "http://a")
        agent = plugin._claude_tool("delegate_task", {"tasks": [{"goal": "look", "context": "ctx", "role": "researcher"}]})
        self.assertEqual(agent[0], "Agent")
        self.assertEqual((agent[1]["prompt"], agent[1]["description"], agent[1]["subagent_type"]), ("look\n\nctx", "look", "researcher"))
        self.assertEqual(plugin._claude_tool("execute_code", {"code": "1"}), ("execute_code", {"code": "1"}))
        self.assertEqual(plugin._claude_tool("terminal", None), ("Bash", {"args": None, "hermes_tool": "terminal"}))

    def test_live_hooks_emit_usage_meta_and_model_change(self):
        sid = "s_hooks"
        plugin._pre_llm_call(session_id=sid, user_message="hi", conversation_history=[], model="m1")
        usage = {"input_tokens": 5, "output_tokens": 2, "cache_read_tokens": 1, "cache_write_tokens": 0, "reasoning_tokens": 0,
                 "request_count": 1, "prompt_tokens": 6, "total_tokens": 8}
        for _ in range(2):
            plugin._post_api_request(session_id=sid, model="m1", usage=usage, ended_at=T0 + 5, api_request_id="req_1")
        plugin._post_tool_call(tool_name="read_file", args={"path": "/a"}, result="x", session_id=sid, tool_call_id="c1")
        plugin._pre_llm_call(session_id=sid, user_message="[System] injected", conversation_history=[{"role": "user"}], model="m2")
        plugin._post_api_request(session_id=sid, model="m2", usage=None, ended_at=T0 + 6)
        plugin._post_llm_call(session_id=sid, assistant_response="ok", conversation_history=[], model="m2")
        plugin._sessions.clear()
        lines = read_lines(plugin._transcript_path(self.home, sid))
        meta = lines.pop(0)
        self.assertEqual((meta["type"], meta["sessionId"], meta["user"]), ("session-meta", sid, plugin._user()))
        self.assertEqual(lines[0]["parentUuid"], meta["uuid"])
        self.assertEqual([l["type"] for l in lines],
                         ["model-change", "user", "usage", "assistant", "user", "model-change", "user", "assistant"])
        self.assertEqual([l["parentUuid"] for l in lines[1:]], [l["uuid"] for l in lines[:-1]])
        self.assertEqual((lines[0]["model"], lines[5]["model"]), ("m1", "m2"))
        self.assertNotIn("message", lines[0])
        self.assertEqual(lines[2]["timestamp"], "2023-11-14T22:13:25.000Z")
        self.assertEqual(lines[2]["usage"], {"input_tokens": 5, "output_tokens": 2, "cache_read_input_tokens": 1,
                                             "cache_creation_input_tokens": 0, "reasoning_tokens": 0})
        self.assertNotIn("costUSD", lines[2])
        tool = lines[3]["message"]["content"][0]
        self.assertEqual((tool["name"], tool["input"]["file_path"], tool["input"]["hermes_tool"]), ("Read", "/a", "read_file"))
        self.assertEqual(lines[6].get("isMeta"), True)
        self.assertNotIn("isMeta", lines[1])
        self.assertEqual(lines[7]["message"]["model"], "m2")

    def test_live_session_meta_carries_source_parent_and_git(self):
        import subprocess
        import tempfile
        with tempfile.TemporaryDirectory() as repo:
            subprocess.run(["git", "init", "-q", "-b", "feat", repo], check=True)
            subprocess.run(["git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"], check=True)
            head = subprocess.run(["git", "-C", repo, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
            old = os.getcwd()
            os.chdir(repo)
            try:
                plugin._on_session_start(session_id="s_child", model="m1", platform="telegram", parent_session_id="s_root")
                plugin._pre_llm_call(session_id="s_child", user_message="hi", conversation_history=[], model="m1")
            finally:
                os.chdir(old)
        plugin._sessions.clear()
        meta = read_lines(plugin._transcript_path(self.home, "s_child"))[0]
        self.assertEqual((meta["type"], meta["agentSource"], meta["parentSessionId"], meta["gitBranch"], meta["gitCommit"]),
                         ("session-meta", "telegram", "s_root", "feat", head))

    def test_backfilled_child_names_its_parent(self):
        if self.use_api:
            self.skipTest("raw schema only")
        conn = sqlite3.connect(self.home / "state.db")
        conn.execute("INSERT INTO sessions(id, source, model, started_at, ended_at, cwd, parent_session_id) VALUES (?,?,?,?,?,?,?)",
                     ("s_kid", "delegate", "m1", T0 + 30, T0 + 40, "/home/dev/proj", "s_ended"))
        conn.execute("INSERT INTO messages(session_id, role, content, timestamp) VALUES (?,?,?,?)", ("s_kid", "user", "sub task", T0 + 31))
        conn.commit()
        conn.close()
        self.run_backfill()
        meta = read_lines(self.tdir / "s_kid.jsonl")[0]
        self.assertEqual((meta["type"], meta["parentSessionId"], meta["agentSource"]), ("session-meta", "s_ended", "delegate"))

    def test_second_run_is_a_noop(self):
        self.run_backfill()
        before = {p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}
        st = self.run_backfill()
        self.assertEqual((st["written"], st["lines"], st["skipped"]), (0, 0, 2))
        self.assertEqual({p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}, before)

    def test_lost_stamp_dedupes_on_uuid(self):
        self.run_backfill()
        before = {p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}
        self.stamp.unlink()
        st = self.run_backfill()
        self.assertEqual(st["written"], 0)
        self.assertEqual({p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}, before)

    def test_legacy_stamp_carries_over_without_a_rebackfill(self):
        self.run_backfill()
        self.legacy.write_bytes(self.stamp.read_bytes())
        self.stamp.unlink()
        legacy = self.legacy.read_bytes()
        before = {p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}
        st = self.run_backfill()
        self.assertEqual((st["written"], st["lines"], st["skipped"]), (0, 0, 2))
        self.assertEqual(json.loads(self.stamp.read_text())["sessions"], json.loads(legacy)["sessions"])
        self.assertEqual(self.legacy.read_bytes(), legacy)
        self.assertEqual({p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}, before)
        self.assertIn("2 sessions backfilled", plugin._slash("status"))

    def test_legacy_stamp_is_read_once(self):
        self.run_backfill()
        mine = json.loads(self.stamp.read_text())["sessions"]
        self.legacy.write_text(json.dumps({"version": 1, "sessions": {"elsewhere": {"skip": "live"}}}))
        self.run_backfill()
        self.assertEqual(json.loads(self.stamp.read_text())["sessions"], mine)

    def test_new_messages_append_incrementally(self):
        self.run_backfill()
        conn = sqlite3.connect(self.home / "state.db")
        conn.execute("INSERT INTO messages(session_id, role, content, timestamp) VALUES (?,?,?,?)",
                     ("s_ended", "user", "one more", T0 + 50))
        conn.execute("INSERT INTO messages(session_id, role, content, timestamp) VALUES (?,?,?,?)",
                     ("s_ended", "assistant", "done", T0 + 51))
        conn.commit()
        conn.close()
        st = self.run_backfill()
        self.assertEqual((st["written"], st["lines"]), (1, 2))
        lines = read_lines(self.tdir / "s_ended.jsonl")
        self.assertEqual(len(lines), 10)
        self.assertEqual(lines[8]["parentUuid"], lines[7]["uuid"])
        self.assertEqual(lines[9]["message"]["content"], [{"type": "text", "text": "done"}])
        conn = sqlite3.connect(self.home / "state.db")
        last_id = conn.execute("SELECT MAX(id) FROM messages WHERE session_id = 's_ended'").fetchone()[0]
        conn.close()
        stamp = json.loads(self.stamp.read_text())
        self.assertEqual(stamp["sessions"]["s_ended"]["last_id"], last_id)
        self.assertEqual(stamp["sessions"]["s_ended"]["size"], (self.tdir / "s_ended.jsonl").stat().st_size)

    def test_live_owned_transcript_is_never_touched(self):
        s = plugin._Session("s_ended", "m1")
        s.append("k", [("user", {"role": "user", "content": "hello there"})])
        plugin._sessions.clear()
        before = s.path.read_bytes()
        st = self.run_backfill()
        self.assertEqual(st["foreign"], 1)
        self.assertEqual(s.path.read_bytes(), before)
        stamp = json.loads(self.stamp.read_text())
        self.assertEqual(stamp["sessions"]["s_ended"], {"skip": "live"})
        st = self.run_backfill()
        self.assertEqual(s.path.read_bytes(), before)

    def test_sessions_owned_by_live_hooks_are_skipped(self):
        plugin._session("s_ended", "m1")
        st = self.run_backfill()
        self.assertEqual([l["type"] for l in read_lines(self.tdir / "s_ended.jsonl")], ["session-meta", "model-change"])
        self.assertEqual(st["written"], 1)

    def test_budget_resumes_next_run(self):
        st = self.run_backfill_with_budget(0)
        self.assertTrue(st["resume"])
        self.assertEqual(st["written"], 0)
        st = self.run_backfill()
        self.assertEqual(st["written"], 2)

    def run_backfill_with_budget(self, budget):
        return plugin.backfill(home=self.home, budget=budget, now=NOW, use_api=self.use_api)

    def test_lock_skips_concurrent_run(self):
        import fcntl
        self.stamp.parent.mkdir(parents=True, exist_ok=True)
        fh = open(self.stamp.parent / plugin.BACKFILL_LOCK, "a+")
        fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            st = self.run_backfill()
        finally:
            fh.close()
        self.assertTrue(st.get("skipped_locked"))
        self.assertEqual(list(self.tdir.glob("*.jsonl")), [])

    def test_slash_and_safe_wrapper(self):
        out = plugin._slash("backfill 0")
        self.assertIn("3 sessions written", out)
        self.assertIn("3 sessions backfilled", plugin._slash("status"))
        self.assertIn("/cccp backfill", plugin._slash(""))
        self.assertIsNotNone(plugin._backfill_safely(budget=float("inf")))

    def test_missing_db_fails_open(self):
        import shutil
        shutil.rmtree(self.home)
        self.assertIsNone(plugin._backfill_safely())

    def test_cli(self):
        rc = plugin.main(["--backfill", str(self.home), "--no-api"])
        self.assertEqual(rc, 0)
        self.assertTrue((self.tdir / "s_ended.jsonl").is_file())


class RawSQLBackfill(BackfillCase, unittest.TestCase):
    use_api = False


@unittest.skipIf(SessionDB is None, "hermes_state not importable")
class HermesAPIBackfill(BackfillCase, unittest.TestCase):
    use_api = True

    def test_api_and_sql_paths_agree(self):
        self.run_backfill(use_api=True)
        api = {p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}
        import shutil
        shutil.rmtree(self.tdir)
        self.run_backfill(use_api=False)
        self.assertEqual({p.name: p.read_bytes() for p in self.tdir.glob("*.jsonl")}, api)



class LegacySchemaSQL(unittest.TestCase):
    """state.db written by an older Hermes: no active/compacted columns (seen 2026-09-08)."""

    def setUp(self):
        import tempfile
        self.tmp = tempfile.TemporaryDirectory()
        self.home = Path(self.tmp.name) / "hermes"
        self.home.mkdir(parents=True)
        os.environ["HERMES_HOME"] = str(self.home)
        conn = sqlite3.connect(self.home / "state.db")
        legacy = SCHEMA.replace(", active INTEGER NOT NULL DEFAULT 1, compacted INTEGER NOT NULL DEFAULT 0", "")
        assert "active" not in legacy.split("CREATE TABLE messages")[1].split(";")[0]
        conn.executescript(legacy)
        insert_usage(conn)
        for s in SESSIONS:
            conn.execute("INSERT INTO sessions(id, source, model, started_at, ended_at, cwd) VALUES (?,?,?,?,?,?)",
                         (s["id"], "cli", s["model"], s["started_at"], s["ended_at"], s["cwd"]))
            for m in MESSAGES.get(s["id"], []):
                conn.execute("INSERT INTO messages(session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, reasoning)"
                             " VALUES (?,?,?,?,?,?,?,?)",
                             (s["id"], m["role"], m.get("content"), m.get("tool_call_id"), m.get("tool_calls"),
                              m.get("tool_name"), m["timestamp"], m.get("reasoning")))
        conn.commit()
        conn.close()
        plugin._sessions.clear()
        self._cccp = plugin._cccp
        plugin._cccp = lambda *a, **k: None

    def tearDown(self):
        plugin._cccp = self._cccp
        self.tmp.cleanup()

    def test_backfill_without_active_column(self):
        st = plugin.backfill(home=self.home, budget=float("inf"), now=NOW, use_api=False)
        self.assertEqual(st.get("errors", 0), 0, st)
        self.assertEqual((st["sessions"], st["written"]), (4, 2), st)
        files = sorted((self.home / "transcripts").glob("*.jsonl"))
        self.assertEqual(len(files), 2)
        kinds = [l["type"] for f in files for l in read_lines(f)]
        self.assertIn("user", kinds)
        self.assertIn("assistant", kinds)

if __name__ == "__main__":
    unittest.main()
