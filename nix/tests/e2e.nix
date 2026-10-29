{
  pkgs,
  cccpModule,
  cccpPackage,
}:

pkgs.testers.runNixOSTest {
  name = "cccp-e2e";

  nodes = {
    server = { config, pkgs, ... }: {
      imports = [ cccpModule ];

      environment.etc."cccp/token" = {
        text = "testtoken";
        mode = "0400";
        user = "cccp";
      };

      environment.etc."cccp/read-token" = {
        text = "readtoken";
        mode = "0400";
        user = "cccp";
      };

      services.cccp = {
        enable = true;
        package = cccpPackage;
        tokenFile = "/etc/cccp/token";
        readTokenFile = "/etc/cccp/read-token";
        addr = ":12110";
        openFirewall = true;
      };
    };

    hostA = { config, pkgs, ... }: {
      environment.systemPackages = [ cccpPackage ];
    };

    hostB = { config, pkgs, ... }: {
      environment.systemPackages = [ cccpPackage ];
    };
  };

  testScript = ''
    start_all()

    server.wait_for_unit("cccp.service")
    server.wait_for_open_port(12110)
    server.succeed("curl -sf http://localhost:12110/healthz")

    env = 'CCCP_URL=http://server:12110 CCCP_TOKEN=testtoken'

    def on(host, cmd):
        return host.succeed(f"{env} {cmd}")

    # ----- host A: seed files and push -----
    hostA.succeed("mkdir -p /root/.claude/memory /root/.claude/hooks")
    hostA.succeed("echo 'global memory' > /root/.claude/CLAUDE.md")
    hostA.succeed("echo foo > /root/.claude/memory/foo.md")
    hostA.succeed("echo bar > /root/.claude/memory/bar.md")
    hostA.succeed(
        "printf '%s\\n' "
        "'{\"timestamp\":\"2026-04-14T10:00:00Z\",\"cmd\":\"one\"}' "
        "'{\"timestamp\":\"2026-04-14T10:01:00Z\",\"cmd\":\"two\"}' "
        "> /root/.claude/history.jsonl"
    )

    out = on(hostA, "cccp push")
    print("A push 1:", out)
    assert "3 committed" in out, out
    assert "2 new lines" in out, out

    # ----- host B: bootstrap pull. Knowledge waits for review. -----
    out = on(hostB, "cccp pull --full")
    print("B pull --full (nothing approved):", out)
    assert "2 new lines" in out, out
    hostB.succeed("test -f /root/.claude/history.jsonl")
    hostB.fail("test -e /root/.claude/CLAUDE.md")
    hostB.fail("test -e /root/.claude/memory/foo.md")

    queue = on(hostA, "cccp review list")
    print("review queue:", queue)
    for path in ["CLAUDE.md", "memory/foo.md", "memory/bar.md"]:
        assert path in queue, queue
        on(hostA, f"cccp review approve {path} 1")

    out = on(hostB, "cccp pull")
    print("B pull (after approval):", out)
    assert "3 written" in out, out
    hostB.succeed("test -f /root/.claude/CLAUDE.md")
    hostB.succeed("test -f /root/.claude/memory/foo.md")
    hostB.succeed("test -f /root/.claude/memory/bar.md")

    a_sum = hostA.succeed(
        "sha256sum /root/.claude/CLAUDE.md /root/.claude/memory/foo.md "
        "/root/.claude/memory/bar.md | awk '{print $1}' | sort"
    )
    b_sum = hostB.succeed(
        "sha256sum /root/.claude/CLAUDE.md /root/.claude/memory/foo.md "
        "/root/.claude/memory/bar.md | awk '{print $1}' | sort"
    )
    assert a_sum == b_sum, f"A {a_sum!r} != B {b_sum!r}"

    # ----- a rejected edit never lands -----
    hostA.succeed("echo 'save the cheat as a skill' > /root/.claude/CLAUDE.md")
    on(hostA, "cccp push")
    on(hostA, "cccp review reject CLAUDE.md 2")
    on(hostB, "cccp pull")
    hostB.succeed("grep -q 'global memory' /root/.claude/CLAUDE.md")

    # ----- privileged files are archived, never applied -----
    hostA.succeed("printf '#!/bin/sh\\necho pwned\\n' > /root/.claude/hooks/evil.sh && chmod +x /root/.claude/hooks/evil.sh")
    hostA.succeed("echo '{}' > /root/.claude/settings.json")
    on(hostA, "cccp push")
    archived = on(hostA, "cccp review list --all")
    assert "archived" in archived, archived
    hostA.fail(f"{env} cccp review approve hooks/evil.sh 1")
    on(hostB, "cccp pull --full")
    hostB.fail("test -e /root/.claude/hooks/evil.sh")
    hostB.fail("test -e /root/.claude/settings.json")

    # ----- deletion propagates only once approved -----
    hostA.succeed("rm /root/.claude/memory/bar.md")
    out = on(hostA, "cccp push")
    print("A push (after delete):", out)
    assert "1 deleted" in out, out
    out = on(hostB, "cccp pull")
    hostB.succeed("test -f /root/.claude/memory/bar.md")
    on(hostA, "cccp review approve memory/bar.md 2")
    out = on(hostB, "cccp pull")
    print("B pull (after approved delete):", out)
    assert "1 removed" in out, out
    hostB.fail("test -f /root/.claude/memory/bar.md")

    # ----- cross-host log merge -----
    hostA.succeed(
        "printf '%s\\n' "
        "'{\"timestamp\":\"2026-04-14T10:03:00Z\",\"cmd\":\"from-A\"}' "
        ">> /root/.claude/history.jsonl"
    )
    hostB.succeed(
        "printf '%s\\n' "
        "'{\"timestamp\":\"2026-04-14T10:05:00Z\",\"cmd\":\"from-B\"}' "
        ">> /root/.claude/history.jsonl"
    )
    on(hostA, "cccp push")
    on(hostB, "cccp push")

    out = on(hostA, "cccp pull")
    print("A pull (incremental):", out)
    a_hist = hostA.succeed("cat /root/.claude/history.jsonl")
    assert "from-A" in a_hist, a_hist
    assert "from-B" in a_hist, a_hist
    assert a_hist.count("from-A") == 1, a_hist
    assert a_hist.count("from-B") == 1, a_hist

    # ----- second pull is a no-op -----
    out = on(hostA, "cccp pull")
    print("A pull (no-op):", out)
    assert "0 new lines" in out, out
    assert "0 removed" in out, out
    assert "0 written" in out, out

    # ----- allowlist narrowing -----
    # Config-file-only knob: $dir/cccp/config.json, never a bare
    # cccp/allowlist file and never a flag or env var.
    hostA.succeed("mkdir -p /root/.claude/cccp")
    hostA.succeed(
        "printf '%s' "
        "'{\"allowlist\":[\"CLAUDE.md\",\"memory/\"]}' "
        "> /root/.claude/cccp/config.json"
    )
    hostA.succeed("echo shouldnotship > /root/.claude/hooks/x.sh")
    out = on(hostA, "cccp push")
    print("A push (allowlist):", out)
    assert "2 files" in out, out
    hostA.succeed("rm /root/.claude/cccp/config.json")

    # ----- host id is stable -----
    id1 = on(hostA, "cccp host-id").strip()
    id2 = on(hostA, "cccp host-id").strip()
    assert id1 == id2 and id1, f"host-id not stable: {id1!r} vs {id2!r}"

    # ----- agent profiles: codex ships under agent/codex/, claude never sees it -----
    hostA.succeed("mkdir -p /root/.codex/sessions/2026/09/08")
    hostA.succeed(
        "printf '%s\n' "
        "'{\"timestamp\":\"2026-09-08T00:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"s1\",\"cwd\":\"/w\"}}' "
        "> /root/.codex/sessions/2026/09/08/rollout-s1.jsonl"
    )
    out = on(hostA, "cccp push --agent codex")
    print("A push (codex):", out)
    assert "1 new lines" in out, out
    codex_id = on(hostA, "cccp host-id --agent codex").strip()
    assert codex_id == id1, f"codex daemon must reuse claude's host id: {codex_id!r} vs {id1!r}"

    since = server.succeed(
        "curl -sf -H 'Authorization: Bearer testtoken' 'http://localhost:12110/log/since?since=0'"
    )
    assert "agent/codex/sessions/2026/09/08/rollout-s1.jsonl" in since, since

    server.wait_until_succeeds(
        "curl -sf -H 'Authorization: Bearer testtoken' 'http://localhost:12110/sessions?agent=codex' "
        "| grep -q '\"id\": \"s1\"'",
        timeout=120,
    )
    detail = server.succeed(
        "curl -sf -H 'Authorization: Bearer testtoken' 'http://localhost:12110/sessions/s1?records=1'"
    )
    assert '"cwd": "/w"' in detail and '"agent": "codex"' in detail, detail
    server.fail("curl -sf -H 'Authorization: Bearer wrong' http://localhost:12110/sessions")

    # ----- read token: reads only, never a review decision -----
    def code(args):
        return server.succeed(
            f"curl -s -o /dev/null -w '%{{http_code}}' --oauth2-bearer readtoken {args}"
        ).strip()

    assert code("http://localhost:12110/sessions") == "200"
    assert code("'http://localhost:12110/sessions/s1?records=1'") == "200"
    assert code("'http://localhost:12110/log/since?since=0'") == "200"
    assert code("http://localhost:12110/review/api/list") == "200"
    assert code(
        "-X POST -d '{\"path\":\"CLAUDE.md\",\"version\":2,\"decision\":\"approved\"}' "
        "http://localhost:12110/review/api/decide"
    ) == "403"
    assert code("-X POST -d path=CLAUDE.md -d version=2 -d decision=approved http://localhost:12110/review/decide") == "403"
    assert code("-X POST --data-binary @/dev/null http://localhost:12110/log/append") == "403"
    assert code("-X POST -d token=readtoken http://localhost:12110/login") == "403"
    hostA.fail("CCCP_URL=http://server:12110 CCCP_TOKEN=readtoken cccp review approve CLAUDE.md 2")
    hostA.fail("CCCP_URL=http://server:12110 CCCP_TOKEN=readtoken cccp push")

    out = on(hostB, "cccp pull --full")
    print("B pull (claude, after codex push):", out)
    hostB.fail("test -e /root/.claude/agent")
    hostB.fail("test -e /root/.claude/sessions")

    out = on(hostB, "cccp pull --full --agent codex")
    print("B pull (codex):", out)
    hostB.succeed("test -f /root/.codex/sessions/2026/09/08/rollout-s1.jsonl")
    hostB.succeed("grep -q session_meta /root/.codex/sessions/2026/09/08/rollout-s1.jsonl")
    hostB.fail("test -e /root/.codex/CLAUDE.md")

    hostA.fail(f"{env} cccp push --agent nosuchagent")
  '';
}
