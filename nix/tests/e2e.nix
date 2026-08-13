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

      # Token file baked into the node for the test. In a real deployment
      # use sops-nix / agenix / systemd-creds instead.
      environment.etc."cccp/token" = {
        text = "testtoken";
        mode = "0400";
        user = "cccp";
      };

      services.cccp = {
        enable = true;
        package = cccpPackage;
        tokenFile = "/etc/cccp/token";
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

    # ----- host B: bootstrap pull -----
    out = on(hostB, "cccp pull --full")
    print("B pull --full:", out)
    assert "3 written" in out, out
    assert "2 new lines" in out, out

    hostB.succeed("test -f /root/.claude/CLAUDE.md")
    hostB.succeed("test -f /root/.claude/memory/foo.md")
    hostB.succeed("test -f /root/.claude/memory/bar.md")
    hostB.succeed("test -f /root/.claude/history.jsonl")

    a_sum = hostA.succeed(
        "sha256sum /root/.claude/CLAUDE.md /root/.claude/memory/foo.md "
        "/root/.claude/memory/bar.md | awk '{print $1}' | sort"
    )
    b_sum = hostB.succeed(
        "sha256sum /root/.claude/CLAUDE.md /root/.claude/memory/foo.md "
        "/root/.claude/memory/bar.md | awk '{print $1}' | sort"
    )
    assert a_sum == b_sum, f"A {a_sum!r} != B {b_sum!r}"

    # ----- deletion propagation -----
    hostA.succeed("rm /root/.claude/memory/bar.md")
    out = on(hostA, "cccp push")
    print("A push 2 (after delete):", out)
    assert "1 deleted" in out, out

    out = on(hostB, "cccp pull")
    print("B pull (after A delete):", out)
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

    # ----- host id is stable -----
    id1 = on(hostA, "cccp host-id").strip()
    id2 = on(hostA, "cccp host-id").strip()
    assert id1 == id2 and id1, f"host-id not stable: {id1!r} vs {id2!r}"
  '';
}
