package config

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CCCP_DIR", "CCCP_URL", "CCCP_TOKEN",
		"CCCP_ADDR", "CCCP_PORT", "CCCP_DB", "CCCP_DEBOUNCE_MS", "CCCP_CONFIG",
		"CCCP_CHEKA_ALLOW", "CCCP_CHEKA_REDACT",
		"HOME",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HOME", t.TempDir())
}

func parseAndResolve(t *testing.T, args []string) *Resolved {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cli := RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	r, err := Resolve(fs, cli)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return r
}

func TestDefaults(t *testing.T) {
	isolateEnv(t)
	r := parseAndResolve(t, nil)
	home, _ := os.UserHomeDir()
	wantDir := filepath.Join(home, ".claude")
	if r.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", r.Dir, wantDir)
	}
	if r.Addr != ":12110" {
		t.Errorf("Addr = %q, want :12110", r.Addr)
	}
	if r.DB != filepath.Join(wantDir, "cccp", "store.sqlite") {
		t.Errorf("DB = %q", r.DB)
	}
	if r.URL != "" || r.Token != "" {
		t.Errorf("URL/Token expected empty, got %q/%q", r.URL, r.Token)
	}
	if r.Allowlist != nil {
		t.Errorf("Allowlist should be nil by default, got %v", r.Allowlist)
	}
}

func TestConfigFileLayer(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"url":"https://file.example","token":"from-file","addr":":9000","allowlist":["CLAUDE.md","memory/"]}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := parseAndResolve(t, []string{"--dir", dir})
	if r.URL != "https://file.example" {
		t.Errorf("URL = %q", r.URL)
	}
	if r.Token != "from-file" {
		t.Errorf("Token = %q", r.Token)
	}
	if r.Addr != ":9000" {
		t.Errorf("Addr = %q", r.Addr)
	}
	want := []string{"CLAUDE.md", "memory/"}
	if !reflect.DeepEqual(r.Allowlist, want) {
		t.Errorf("Allowlist = %v, want %v", r.Allowlist, want)
	}
	if r.Source != cfgPath {
		t.Errorf("Source = %q, want %q", r.Source, cfgPath)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"url":"https://file.example","token":"ft"}`), 0o600)

	t.Setenv("CCCP_DIR", dir)
	t.Setenv("CCCP_URL", "https://env.example")
	t.Setenv("CCCP_TOKEN", "from-env")

	r := parseAndResolve(t, nil)
	if r.URL != "https://env.example" {
		t.Errorf("URL = %q, want env value", r.URL)
	}
	if r.Token != "from-env" {
		t.Errorf("Token = %q", r.Token)
	}
}

func TestCLIBeatsEnvBeatsFile(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"url":"https://file","token":"ft","addr":":1"}`), 0o600)

	t.Setenv("CCCP_DIR", dir)
	t.Setenv("CCCP_URL", "https://env")
	t.Setenv("CCCP_ADDR", ":2")

	r := parseAndResolve(t, []string{"--url", "https://cli", "--addr", ":3"})
	if r.URL != "https://cli" {
		t.Errorf("URL = %q, want cli", r.URL)
	}
	if r.Addr != ":3" {
		t.Errorf("Addr = %q, want :3", r.Addr)
	}
}

func TestEmptyFlagOverridesLowerLayer(t *testing.T) {
	isolateEnv(t)
	t.Setenv("CCCP_URL", "https://env.example")
	r := parseAndResolve(t, []string{"--url", ""})
	if r.URL != "" {
		t.Errorf("URL = %q, want empty (CLI explicit override)", r.URL)
	}
}

func TestEmptyEnvOverridesFile(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"url":"https://file"}`), 0o600)

	t.Setenv("CCCP_DIR", dir)
	t.Setenv("CCCP_URL", "")

	r := parseAndResolve(t, nil)
	if r.URL != "" {
		t.Errorf("URL = %q, want empty (env explicit empty)", r.URL)
	}
}

func TestAliases(t *testing.T) {
	isolateEnv(t)
	cases := [][]string{
		{"--server-url", "https://alias"},
		{"--address", ":5555"},
		{"--database", "/tmp/a.sqlite"},
		{"--db-path", "/tmp/b.sqlite"},
	}
	for _, args := range cases {
		r := parseAndResolve(t, args)
		switch args[0] {
		case "--server-url":
			if r.URL != "https://alias" {
				t.Errorf("%v: URL = %q", args, r.URL)
			}
		case "--address":
			if r.Addr != ":5555" {
				t.Errorf("%v: Addr = %q", args, r.Addr)
			}
		case "--database", "--db-path":
			if r.DB != args[1] {
				t.Errorf("%v: DB = %q", args, r.DB)
			}
		}
	}
}

func TestPortOverridesAddrPort(t *testing.T) {
	isolateEnv(t)
	r := parseAndResolve(t, []string{"--addr", "0.0.0.0:1111", "--port", "2222"})
	if r.Addr != "0.0.0.0:2222" {
		t.Errorf("Addr = %q, want 0.0.0.0:2222", r.Addr)
	}
}

func TestPortOnDefaultAddr(t *testing.T) {
	isolateEnv(t)
	r := parseAndResolve(t, []string{"--port", "9090"})
	if r.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", r.Addr)
	}
}

func TestExplicitConfigNotFoundErrors(t *testing.T) {
	isolateEnv(t)
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cli := RegisterFlags(fs)
	if err := fs.Parse([]string{"--config", "/nope/nowhere.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(fs, cli); err == nil {
		t.Fatal("expected error for missing explicit --config path")
	}
}

func TestMissingImplicitConfigIsFine(t *testing.T) {
	isolateEnv(t)
	r := parseAndResolve(t, []string{"--dir", t.TempDir()})
	if r.Source != "" {
		t.Errorf("Source should be empty when no config file exists, got %q", r.Source)
	}
}

func TestDebounceLayering(t *testing.T) {
	isolateEnv(t)

	r := parseAndResolve(t, nil)
	if r.DebounceMS != 500 {
		t.Errorf("default DebounceMS = %d, want 500", r.DebounceMS)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"debounce_ms":250}`), 0o600)
	r = parseAndResolve(t, []string{"--dir", dir})
	if r.DebounceMS != 250 {
		t.Errorf("file DebounceMS = %d, want 250", r.DebounceMS)
	}

	t.Setenv("CCCP_DIR", dir)
	t.Setenv("CCCP_DEBOUNCE_MS", "750")
	r = parseAndResolve(t, nil)
	if r.DebounceMS != 750 {
		t.Errorf("env DebounceMS = %d, want 750", r.DebounceMS)
	}

	r = parseAndResolve(t, []string{"--debounce-ms", "1000"})
	if r.DebounceMS != 1000 {
		t.Errorf("cli DebounceMS = %d, want 1000", r.DebounceMS)
	}

	r = parseAndResolve(t, []string{"--debounce-ms", "0"})
	if r.DebounceMS != 0 {
		t.Errorf("explicit 0 DebounceMS = %d, want 0", r.DebounceMS)
	}
}

func TestAddrWithSchemeBecomesURL(t *testing.T) {
	isolateEnv(t)

	r := parseAndResolve(t, []string{"--addr", "http://localhost:9876"})
	if r.URL != "http://localhost:9876" {
		t.Errorf("URL = %q, want http://localhost:9876", r.URL)
	}
	if r.Addr != ":12110" {
		t.Errorf("Addr = %q, want :12110 (reset to default)", r.Addr)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"url":"https://prod.example"}`), 0o600)
	r = parseAndResolve(t, []string{"--dir", dir, "--addr", "http://localhost:9876"})
	if r.URL != "http://localhost:9876" {
		t.Errorf("URL = %q, want http://localhost:9876 (CLI --addr overrides config url)", r.URL)
	}

	os.WriteFile(cfgPath, []byte(`{"addr":"http://bad-config"}`), 0o600)
	r = parseAndResolve(t, []string{"--dir", dir, "--url", "https://cli.example"})
	if r.URL != "https://cli.example" {
		t.Errorf("URL = %q, want https://cli.example (CLI --url takes precedence)", r.URL)
	}
	if r.Addr != ":12110" {
		t.Errorf("Addr = %q, want :12110 (http scheme in addr always resets)", r.Addr)
	}

	os.WriteFile(cfgPath, []byte(`{}`), 0o600)
	t.Setenv("CCCP_ADDR", "https://env.example:8443")
	r = parseAndResolve(t, []string{"--dir", dir})
	if r.URL != "https://env.example:8443" {
		t.Errorf("URL = %q, want https://env.example:8443", r.URL)
	}
	if r.Addr != ":12110" {
		t.Errorf("Addr = %q, want :12110", r.Addr)
	}
}

func TestAddrWithoutSchemeUnchanged(t *testing.T) {
	isolateEnv(t)

	r := parseAndResolve(t, []string{"--addr", "0.0.0.0:9876"})
	if r.Addr != "0.0.0.0:9876" {
		t.Errorf("Addr = %q, want 0.0.0.0:9876", r.Addr)
	}
	if r.URL != "" {
		t.Errorf("URL = %q, want empty", r.URL)
	}
}

func TestChekaAllowLayering(t *testing.T) {
	isolateEnv(t)

	r := parseAndResolve(t, nil)
	if r.ChekaAllow != 0 {
		t.Errorf("default ChekaAllow = %d, want 0", r.ChekaAllow)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"cheka_allow":3}`), 0o600)
	r = parseAndResolve(t, []string{"--dir", dir})
	if r.ChekaAllow != 3 {
		t.Errorf("file ChekaAllow = %d, want 3", r.ChekaAllow)
	}

	t.Setenv("CCCP_DIR", dir)
	t.Setenv("CCCP_CHEKA_ALLOW", "5")
	r = parseAndResolve(t, nil)
	if r.ChekaAllow != 5 {
		t.Errorf("env ChekaAllow = %d, want 5", r.ChekaAllow)
	}

	r = parseAndResolve(t, []string{"--cheka-allow", "7"})
	if r.ChekaAllow != 7 {
		t.Errorf("cli ChekaAllow = %d, want 7", r.ChekaAllow)
	}

	r = parseAndResolve(t, []string{"--cheka-allow", "-1"})
	if r.ChekaAllow != -1 {
		t.Errorf("disable sentinel lost: ChekaAllow = %d, want -1", r.ChekaAllow)
	}
}

func TestChekaRedactLayering(t *testing.T) {
	isolateEnv(t)

	r := parseAndResolve(t, nil)
	if r.ChekaRedact {
		t.Errorf("default ChekaRedact = true, want false")
	}
	if r.ChekaRedactSet {
		t.Errorf("default ChekaRedactSet = true, want false (no layer set it)")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"cheka_redact":false}`), 0o600)
	r = parseAndResolve(t, []string{"--dir", dir})
	if r.ChekaRedact {
		t.Errorf("file explicit-false ChekaRedact = true, want false")
	}
	if !r.ChekaRedactSet {
		t.Errorf("file explicit-false should set ChekaRedactSet; got false")
	}

	os.WriteFile(cfgPath, []byte(`{"cheka_redact":true}`), 0o600)
	r = parseAndResolve(t, []string{"--dir", dir})
	if !r.ChekaRedact || !r.ChekaRedactSet {
		t.Errorf("file ChekaRedact=true: got %v set=%v", r.ChekaRedact, r.ChekaRedactSet)
	}

	t.Setenv("CCCP_DIR", dir)
	t.Setenv("CCCP_CHEKA_REDACT", "false")
	r = parseAndResolve(t, nil)
	if r.ChekaRedact {
		t.Errorf("env=false did not override file=true")
	}
	if !r.ChekaRedactSet {
		t.Errorf("env=false should still set ChekaRedactSet")
	}

	r = parseAndResolve(t, []string{"--cheka-redact"})
	if !r.ChekaRedact {
		t.Errorf("cli --cheka-redact did not override env=false")
	}
}

func TestChekaRedactBadEnvErrors(t *testing.T) {
	isolateEnv(t)
	t.Setenv("CCCP_CHEKA_REDACT", "not-a-bool")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cli := RegisterFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(fs, cli); err == nil {
		t.Fatal("expected parse error for bogus CCCP_CHEKA_REDACT")
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cccp", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`{"nonsense":"x"}`), 0o600)

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cli := RegisterFlags(fs)
	if err := fs.Parse([]string{"--dir", dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(fs, cli); err == nil {
		t.Fatal("expected error for unknown config field")
	}
}
