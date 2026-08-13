package cheka

import (
	"strings"
	"testing"
)

const realChars = "AbC1dE2fG3hI4jK5lM6nO7pQ8rS9tU0vWxYzBc2dE4fG6hI8jK0lM3nO5pQ7rS9tU1vW3xY5z"

func rep(s string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(s)
	}
	return b.String()[:n]
}

func TestScanLine_ServiceKeys(t *testing.T) {
	cases := []struct {
		name string
		line string
		kind string
	}{
		{
			"anthropic",
			`export ANTHROPIC_API_KEY=sk-ant-api03-` + rep(realChars, 93) + `AA`,
			"anthropic_key",
		},
		{
			"anthropic_admin",
			`sk-ant-admin01-` + rep(realChars, 95),
			"anthropic_key",
		},
		{
			"openai_project",
			`OPENAI_KEY=sk-proj-` + rep(realChars, 48),
			"openai_key",
		},
		{
			"openai_legacy",
			`OPENAI_KEY=sk-` + rep(realChars, 48),
			"openai_legacy_key",
		},
		{
			"openai_svcacct",
			`sk-svcacct-` + rep(realChars, 48),
			"openai_key",
		},
		{
			"github_classic",
			`token: ghp_` + rep(realChars, 36),
			"github_classic_token",
		},
		{
			"github_fine_grained",
			`GITHUB_TOKEN=github_pat_` + rep(realChars, 22) + `_` + rep(realChars, 59),
			"github_fine_grained_pat",
		},
		{
			"gitlab",
			`GITLAB=glpat-` + rep(realChars, 24),
			"gitlab_token",
		},
		{
			"huggingface",
			`HF_TOKEN=hf_` + rep(realChars, 34),
			"huggingface_key",
		},
		{
			"slack_bot",
			`SLACK=xoxb-` + rep("7258493019", 1) + `-` + rep("8391027465", 1) + `-` + rep(realChars, 24),
			"slack_token",
		},
		{
			"slack_webhook",
			`url=https://hooks.slack.com/services/T01234ABC/B56789XYZ/abcdefgh01234567`,
			"slack_webhook",
		},
		{
			"stripe_live",
			`sk_live_` + rep(realChars, 32),
			"stripe_key",
		},
		{
			"google_api",
			`GOOGLE_KEY=AIza` + rep(realChars, 35),
			"google_api_key",
		},
		{
			"google_oauth",
			`ya29.` + rep(realChars, 60),
			"google_oauth_token",
		},
		{
			"npm",
			`npm_` + rep(realChars, 36),
			"npm_token",
		},
		{
			"sendgrid",
			`SG.` + rep(realChars, 22) + `.` + rep(realChars, 43),
			"sendgrid_key",
		},
		{
			"docker_hub",
			`dckr_pat_` + rep(realChars, 28),
			"docker_hub_token",
		},
		{
			"digitalocean",
			`dop_v1_` + rep("a1b2c3d4e5f6", 64),
			"digitalocean_token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ff := ScanLine("test.txt", tc.line, 1)
			if len(ff) == 0 {
				t.Fatalf("expected finding for %q, got none", tc.line)
			}
			found := false
			for _, f := range ff {
				if f.Kind == tc.kind {
					found = true
				}
			}
			if !found {
				t.Errorf("expected kind %q, got %v", tc.kind, ff)
			}
		})
	}
}

func TestScanLine_JWT(t *testing.T) {
	jwt := `eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c`
	ff := ScanLine("test.txt", jwt, 1)
	if len(ff) == 0 {
		t.Fatal("expected JWT finding")
	}
	if ff[0].Kind != "jwt" {
		t.Errorf("expected kind jwt, got %s", ff[0].Kind)
	}
}

func TestScanLine_AWSKey(t *testing.T) {
	ff := ScanLine("test.txt", `aws_access_key_id = AKIAQX3SJOD7WPXK5QWZ`, 1)
	if len(ff) == 0 {
		t.Fatal("expected AWS key finding")
	}
	found := false
	for _, f := range ff {
		if f.Kind == "aws_access_key" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected kind aws_access_key, got %v", ff)
	}
}

func TestScanLine_AWSExample_Skipped(t *testing.T) {

	cases := []string{
		`aws_access_key_id = AKIAIOSFODNN7EXAMPLE`,
		`aws_secret = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
	}
	for _, line := range cases {
		ff := ScanLine("test.txt", line, 1)
		for _, f := range ff {
			if f.Kind == "aws_access_key" {
				t.Errorf("unexpected aws_access_key finding for example %q: %v", line, f)
			}
		}
	}
}

func TestScanLine_PrivateKey(t *testing.T) {
	ff := ScanLine("test.txt", `-----BEGIN RSA PRIVATE KEY-----`, 1)
	if len(ff) == 0 {
		t.Fatal("expected private key finding")
	}
	if ff[0].Kind != "private_key" {
		t.Errorf("expected kind private_key, got %s", ff[0].Kind)
	}
}

func TestScanLine_ConnectionString(t *testing.T) {
	ff := ScanLine("test.txt", `DATABASE_URL=postgres://dbuser:Xr7p2qL9aVnB@db.internal.corp:5432/mydb`, 1)
	if len(ff) == 0 {
		t.Fatal("expected connection string finding")
	}
	found := false
	for _, f := range ff {
		if f.Kind == "connection_string" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected kind connection_string, got %v", ff)
	}
}

func TestScanLine_ConnectionString_TemplateSkipped(t *testing.T) {

	cases := []string{
		`const url = ` + "`postgres://${dbUser}:${dbPassword}@127.0.0.1:5432/db`",
		`url = "postgres://{{user}}:{{pass}}@host/db"`,
		`url = "postgres://" + user + ":" + process.env.DB_PASS + "@host/db"`,
	}
	for _, line := range cases {
		ff := ScanLine("test.txt", line, 1)
		for _, f := range ff {
			if f.Kind == "connection_string" {
				t.Errorf("unexpected connection_string finding for %q: %v", line, f)
			}
		}
	}
}

func TestScanLine_Bearer(t *testing.T) {
	ff := ScanLine("test.txt", `Authorization: Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV`, 1)
	found := false
	for _, f := range ff {
		if f.Kind == "bearer" || f.Kind == "jwt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected bearer or jwt finding, got %v", ff)
	}
}

func TestScanLine_Bearer_ExampleSkipped(t *testing.T) {
	cases := []string{
		`Bearer invalid-token-that-does-not-exist`,
		`Bearer your-api-token-goes-here`,
		`Bearer <YOUR_TOKEN_HERE_PLEASE>`,
	}
	for _, line := range cases {
		ff := ScanLine("test.txt", line, 1)
		for _, f := range ff {
			if f.Kind == "bearer" {
				t.Errorf("unexpected bearer finding for %q: %v", line, f)
			}
		}
	}
}

func TestScanLine_KeyValue(t *testing.T) {
	cases := []string{
		`PASSWORD=Xr7p2qL9aVnBcD3eFgH4`,
		`api_key: "Xy9zZ1234LongTokenABC"`,
		`SECRET=Tb4jQr7p2qL9aVnBcD3eFgH4`,
	}
	for _, line := range cases {
		ff := ScanLine("test.txt", line, 1)
		if len(ff) == 0 {
			t.Errorf("expected finding for %q, got none", line)
		}
	}
}

func TestScanLine_KeyValue_DictionarySkipped(t *testing.T) {

	cases := []string{
		`const secret = registrationSecret;`,
		`function foo(secret: string) {}`,
		`const token = process.env.GITHUB_TOKEN;`,
		`secret = ${webhookSecret}`,
		`password: 'changeme'`,
		`api_key: "your-api-key-here"`,
		`PASSWORD=password`,
		`SECRET=secret`,
		`TOKEN=null`,
		`SECRET=undefined`,
		`api_key: example`,
	}
	for _, line := range cases {
		ff := ScanLine("test.txt", line, 1)
		for _, f := range ff {
			if f.Kind == "key_value" {
				t.Errorf("unexpected key_value finding for %q: %v", line, f)
			}
		}
	}
}

func TestScanLine_AnthropicKey_Regression(t *testing.T) {

	line := `export ANTHROPIC_API_KEY=sk-ant-api03-` + rep(realChars, 93) + `AA`
	ff := ScanLine("test.txt", line, 1)
	found := false
	for _, f := range ff {
		if f.Kind == "anthropic_key" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected anthropic_key finding, got %v", ff)
	}
}

func TestScanLine_Clean(t *testing.T) {
	clean := []string{
		`just a normal log line`,
		`{"message": "hello world", "level": "info"}`,
		`func main() { fmt.Println("hello") }`,
		`// this is a comment`,
		`const config = { secret: string, token: boolean };`,
		`if (password.length < 8) throw new Error("too short")`,
		`The API key is stored in Vault under kv/prod/api_key`,
		`interface Credentials { password: string; apiKey: string }`,
	}
	for _, line := range clean {
		ff := ScanLine("test.txt", line, 1)
		if len(ff) != 0 {
			t.Errorf("expected no findings for %q, got %v", line, ff)
		}
	}
}

func TestScan_Binary(t *testing.T) {
	content := []byte{0xff, 0xfe, 0x00, 0x01, 0x02}
	ff := Scan("binary.dat", content)
	if len(ff) != 0 {
		t.Errorf("expected no findings for binary content, got %d", len(ff))
	}
}

func TestScanLine_SafeHex(t *testing.T) {

	cases := []string{
		`commit abc123def456abc123def456abc123def456abcd`,
		`sha256: abc123def456abc123def456abc123def456abc123def456abc123def456abcd`,
		`ref abc123def456abc123def456abc123def456abc123def4`,
		`mac 0123456789abcdef0123456789abcdef`,
	}
	for _, line := range cases {
		ff := ScanLine("test.txt", line, 1)
		for _, f := range ff {
			if f.Kind == "hex_token" {
				t.Errorf("unexpected hex_token finding for %q: %v", line, f)
			}
		}
	}
}

func TestScanLine_LongHex(t *testing.T) {

	high := `raw f3a91c0d2b7e4a8f1d3c52b9e6f0a71b4c8d92e5f6a3b1c7d4e8f09a2b5c1d3e4f09a2`
	ff := ScanLine("test.txt", high, 1)
	found := false
	for _, f := range ff {
		if f.Kind == "hex_token" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected hex_token finding for high-entropy 70-char hex, got %v", ff)
	}
}

func TestScanLine_LowEntropyHexSkipped(t *testing.T) {

	lines := []string{
		`raw: 000000000000000000000000000000000000000000000000000000000000000000000000`,
		`dump: 0c02000004000000b70000ff00000000b70000e400000000b70000` + strings.Repeat("00", 20),
	}
	for _, line := range lines {
		ff := ScanLine("test.txt", line, 1)
		for _, f := range ff {
			if f.Kind == "hex_token" {
				t.Errorf("unexpected hex_token finding for low-entropy %q: %v", line, f)
			}
		}
	}
}

func TestRedactLine_Secrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"openai_legacy",
			`export OPENAI_KEY=sk-` + strings.Repeat("A", 48),
			`export OPENAI_KEY=[REDACTED:openai_legacy_key]`,
		},
		{
			"jwt",
			`auth eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U`,
			`auth [REDACTED:jwt]`,
		},
		{
			"aws_key",
			`aws_access_key_id = AKIAQX3SJOD7WPXK5QWZ`,
			`aws_access_key_id = [REDACTED:aws_access_key]`,
		},
		{
			"aws_example_passes_through",
			`aws_access_key_id = AKIAIOSFODNN7EXAMPLE`,
			`aws_access_key_id = AKIAIOSFODNN7EXAMPLE`,
		},
		{
			"connection_string",
			`DATABASE_URL=postgres://dbuser:Xr7p2qL9aVnB@db.internal.corp:5432/mydb`,
			`DATABASE_URL=[REDACTED:connection_string]`,
		},
		{
			"key_value",
			`PASSWORD=Xr7p2qL9aVnBcD3eFgH4`,
			`[REDACTED:key_value]`,
		},
		{
			"clean",
			`just a normal log line`,
			`just a normal log line`,
		},
		{
			"safe_hex_untouched",
			`commit abc123def456abc123def456abc123def456abcd`,
			`commit abc123def456abc123def456abc123def456abcd`,
		},
		{
			"template_literal_untouched",
			`url = "postgres://${user}:${pass}@host/db"`,
			`url = "postgres://${user}:${pass}@host/db"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactLine(tc.in)
			if got != tc.want {
				t.Errorf("RedactLine(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactLine_Multiple(t *testing.T) {
	line := `PASSWORD=Xr7p2qL9aVnBcD3eFgH4 and ghp_` + rep(realChars, 36)
	got := RedactLine(line)
	if got == line {
		t.Fatal("expected redaction, got original line back")
	}
	if strings.Contains(got, "Xr7p2qL9aVnBcD3eFgH4") {
		t.Error("password not redacted")
	}
	if strings.Contains(got, "ghp_") {
		t.Error("github token not redacted")
	}
	if !strings.Contains(got, "[REDACTED:") {
		t.Errorf("expected redaction marker, got %q", got)
	}
}

func FuzzScanLine(f *testing.F) {
	seeds := []string{
		"",
		"normal log line",
		`PASSWORD=xxxxxxxx`,
		`sk-` + strings.Repeat("a", 40),
		`eyJhbGciOi` + strings.Repeat("X", 40) + `.eyJzdWIi` + strings.Repeat("Y", 40) + `.sig` + strings.Repeat("Z", 40),
		strings.Repeat("a", 200),
		"\x00\x01\xff\xfe",
		"line\nwith\nnewlines",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		findings := ScanLine("fuzz.txt", s, 1)
		for _, fd := range findings {
			if fd.Line < 1 {
				t.Errorf("negative line number: %+v", fd)
			}
			if fd.Offset < 0 || fd.Offset > len(s) {
				t.Errorf("offset %d out of range [0,%d] for input len %d: %+v", fd.Offset, len(s), len(s), fd)
			}
			if fd.Kind == "" {
				t.Errorf("empty kind: %+v", fd)
			}
		}
	})
}

func FuzzRedactLine(f *testing.F) {
	f.Add("normal line")
	f.Add(`PASSWORD=hunter2_secret`)
	f.Add(`Bearer ` + strings.Repeat("x", 30))
	f.Fuzz(func(t *testing.T, s string) {
		out := RedactLine(s)

		for _, fd := range ScanLine("fuzz.txt", s, 1) {
			if fd.Offset+len(fd.Match) <= len(s) {
				raw := s[fd.Offset : fd.Offset+len(fd.Match)]
				if strings.HasSuffix(raw, "...") {
					continue
				}
				if strings.Contains(out, raw) {
					t.Errorf("redacted output still contains secret %q (kind %s)\ninput: %q\noutput: %q",
						raw, fd.Kind, s, out)
				}
			}
		}
	})
}

func TestScan_MultiLine(t *testing.T) {
	content := "line one is clean\n" +
		"PASSWORD=Xr7p2qL9aVnBcD3eFgH4\n" +
		"line three has ghp_" + rep(realChars, 36) + "\n"
	ff := Scan("config.txt", []byte(content))
	if len(ff) < 2 {
		t.Fatalf("expected at least 2 findings, got %d: %v", len(ff), ff)
	}
	for _, f := range ff {
		if f.Line < 1 || f.Line > 3 {
			t.Errorf("unexpected line number %d", f.Line)
		}
	}
}
