package cheka

import (
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Finding struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Match  string `json:"match"`
	Line   int    `json:"line"`
	Offset int    `json:"offset"`
}

type pattern struct {
	kind    string
	re      *regexp.Regexp
	generic bool
}

var patterns = []pattern{

	{"anthropic_key", regexp.MustCompile(`sk-ant-(?:api|admin)\d+-[A-Za-z0-9_\-]{80,}`), false},

	{"openai_key", regexp.MustCompile(`sk-(?:proj|svcacct|admin|None)[-_][A-Za-z0-9]{40,}`), false},

	{"openai_legacy_key", regexp.MustCompile(`sk-[A-Za-z0-9]{40,}`), false},

	{"github_classic_token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36}`), false},

	{"github_fine_grained_pat", regexp.MustCompile(`github_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{59}`), false},

	{"gitlab_token", regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{20,}`), false},

	{"slack_webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`), false},
	{"slack_token", regexp.MustCompile(`xox[baporsz]-\d+-\d+-[A-Za-z0-9\-]+`), false},

	{"stripe_key", regexp.MustCompile(`(?:sk|rk|pk)_(?:live|test)_[0-9a-zA-Z]{24,}`), false},

	{"google_api_key", regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`), false},

	{"google_oauth_token", regexp.MustCompile(`ya29\.[0-9A-Za-z_\-]{20,}`), false},

	{"huggingface_key", regexp.MustCompile(`hf_[A-Za-z0-9]{30,}`), false},

	{"npm_token", regexp.MustCompile(`npm_[A-Za-z0-9]{36}`), false},

	{"sendgrid_key", regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{22}\.[A-Za-z0-9_\-]{43}`), false},

	{"aws_access_key", regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), false},

	{"docker_hub_token", regexp.MustCompile(`dckr_pat_[A-Za-z0-9_\-]{20,}`), false},

	{"digitalocean_token", regexp.MustCompile(`dop_v1_[a-f0-9]{64}`), false},

	{"hasura_admin_secret", regexp.MustCompile(`hskey-auth-[a-zA-Z0-9]{20,}`), false},

	{"synadia_token", regexp.MustCompile(`syt_[A-Za-z0-9_\-]{20,}`), false},

	{"private_key", regexp.MustCompile(`-----BEGIN\s+(?:RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----`), false},

	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{20,}\.eyJ[A-Za-z0-9_\-]{20,}\.[A-Za-z0-9_\-]{20,}`), false},

	{"connection_string", regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^:@\s/]{1,64}:([^@\s/]{1,256})@[^\s]{1,256}`), true},

	{"bearer", regexp.MustCompile(`(?i)Bearer\s+([A-Za-z0-9_\-.]{20,})`), true},

	{"key_value", regexp.MustCompile(`(?i)(?:PASSWORD|PASSWD|TOKEN|SECRET|API[_ ]?KEY|ACCESS[_ ]?KEY|PRIVATE[_ ]?KEY|AUTH[_ ]?TOKEN)\s*[=:]\s*["']?([^\s"'\n]{8,})`), true},

	{"hex_token", regexp.MustCompile(`(?i)[0-9a-f]{65,}`), true},
}

var knownExamples = map[string]bool{
	"akiaiosfodnn7example":                     true,
	"wjalrxutnfemi/k7mdeng/bpxrficyexamplekey": true,
	"akiaxxxxxxxxxxxxxxxx":                     true,
	"aidaxxxxxxxxxxxxxxxx":                     true,
}

var placeholderMarkers = []string{
	"example",
	"changeme",
	"placeholder",
	"redacted",
	"your-", "your_",
	"yourkey", "yoursecret", "yourtoken", "yourapi", "yourpass",
	"<your", "<api", "<secret", "<token", "<pass", "<key",
	"insert-", "replace-", "replace_", "replace-this",
	"invalid-",
	"not-a-real-", "not_a_real",
	"fake-", "dummy",
	"xxxxxxxx", "yyyyyyyy", "zzzzzzzz",
	"todo-", "todo_",
	"...",

	"abcdefghij", "0123456789",
}

var dictionaryWords = map[string]bool{
	"password": true, "passwd": true, "pass": true,
	"secret": true, "token": true, "apikey": true, "api_key": true,
	"key": true, "auth": true, "authorization": true,
	"example": true, "sample": true, "demo": true, "default": true,
	"changeme": true, "hunter2": true, "p4ssw0rd": true,
	"placeholder": true, "redacted": true, "sensitive": true,
	"dummy": true, "fake": true, "test": true, "testing": true,
	"true": true, "false": true, "null": true, "nil": true,
	"none": true, "undefined": true, "void": true,
	"string": true, "number": true, "boolean": true, "integer": true,
	"object": true, "array": true, "buffer": true,
	"public": true, "private": true, "protected": true,
	"admin": true, "root": true, "user": true, "username": true,
	"foo": true, "bar": true, "baz": true, "qux": true,
	"todo": true, "fixme": true, "xxx": true,
	"readonly": true, "required": true, "optional": true,
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]*$`)

func isBenign(eval, fullMatch string, strict bool) bool {
	v := strings.TrimSpace(eval)

	for _, esc := range []string{`\n`, `\t`, `\r`, `\\`, `\"`} {
		if idx := strings.Index(v, esc); idx >= 0 {
			v = v[:idx]
		}
	}
	v = strings.Trim(v, "\"'`,;:)(]}[>{<\\")

	if len(v) < 8 {
		return true
	}
	lv := strings.ToLower(v)

	if knownExamples[lv] {
		return true
	}
	for _, m := range placeholderMarkers {
		if strings.Contains(lv, m) {
			return true
		}
	}

	if fullMatch != "" && fullMatch != eval {
		lf := strings.ToLower(fullMatch)
		for _, m := range placeholderMarkers {
			if strings.Contains(lf, m) {
				return true
			}
		}
	}

	if strings.Contains(v, "${") ||
		strings.Contains(v, "{{") ||
		strings.Contains(v, "%(") ||
		strings.Contains(v, "<%") ||
		strings.Contains(v, "%s") ||
		strings.Contains(v, "%d") {
		return true
	}

	if strings.Contains(v, "process.env") ||
		strings.Contains(v, "os.Getenv") ||
		strings.Contains(v, "os.environ") ||
		strings.Contains(v, "System.getenv") ||
		strings.Contains(v, "getenv(") {
		return true
	}

	if len(v) >= 2 && v[0] == '$' {
		c := v[1]
		if c == '_' || (c >= 'A' && c <= 'Z') {
			return true
		}
	}

	if strings.ContainsAny(v, "(){}[]") {
		return true
	}

	if strings.HasPrefix(v, "eyJ") {
		if parts := strings.Split(v, "."); len(parts) == 3 {
			if len(parts[1]) < 20 || len(parts[2]) < 10 {
				return true
			}
		}
	}
	if strict {
		return false
	}

	if dictionaryWords[lv] {
		return true
	}
	if looksLikeIdentifier(v) {
		return true
	}
	if len(v) >= 8 && shannonEntropy(v) < 2.8 {
		return true
	}
	return false
}

func looksLikeIdentifier(v string) bool {
	if !identRe.MatchString(v) {
		return false
	}
	if len(v) >= 40 {
		return false
	}
	for _, r := range v {
		if unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]int
	byteLen := 0
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
		byteLen++
	}
	n := float64(byteLen)
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

func Scan(path string, content []byte) []Finding {
	if !utf8.Valid(content) {
		return nil
	}
	var out []Finding
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		out = append(out, scanLine(path, line, i+1)...)
	}
	return out
}

func ScanLine(path string, line string, lineNum int) []Finding {
	return scanLine(path, line, lineNum)
}

func scanLine(path, line string, lineNum int) []Finding {
	if len(line) == 0 {
		return nil
	}
	type span struct{ start, end int }
	var claimed []span
	overlaps := func(a span) bool {
		for _, b := range claimed {
			if a.start < b.end && b.start < a.end {
				return true
			}
		}
		return false
	}
	var out []Finding
	for _, p := range patterns {
		locs := p.re.FindAllStringSubmatchIndex(line, -1)
		for _, loc := range locs {
			matchStart, matchEnd := loc[0], loc[1]

			if overlaps(span{matchStart, matchEnd}) {
				continue
			}
			matched := line[matchStart:matchEnd]

			evalText := matched
			if len(loc) >= 4 && loc[2] >= 0 {
				evalText = line[loc[2]:loc[3]]
			}
			if isBenign(evalText, matched, !p.generic) {
				continue
			}
			claimed = append(claimed, span{matchStart, matchEnd})
			out = append(out, Finding{
				Path:   path,
				Kind:   p.kind,
				Match:  truncate(matched, 40),
				Line:   lineNum,
				Offset: matchStart,
			})
		}
	}
	return out
}

func RedactLine(line string) string {
	type span struct {
		start, end int
		kind       string
	}
	var spans []span
	for _, p := range patterns {
		locs := p.re.FindAllStringSubmatchIndex(line, -1)
		for _, loc := range locs {
			matched := line[loc[0]:loc[1]]
			evalText := matched
			if len(loc) >= 4 && loc[2] >= 0 {
				evalText = line[loc[2]:loc[3]]
			}
			if isBenign(evalText, matched, !p.generic) {
				continue
			}
			spans = append(spans, span{loc[0], loc[1], p.kind})
		}
	}
	if len(spans) == 0 {
		return line
	}

	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && (spans[j].start < spans[j-1].start ||
			(spans[j].start == spans[j-1].start && spans[j].end > spans[j-1].end)); j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	var b strings.Builder
	pos := 0
	for _, s := range spans {
		if s.start < pos {
			continue
		}
		b.WriteString(line[pos:s.start])
		b.WriteString("[REDACTED:")
		b.WriteString(s.kind)
		b.WriteByte(']')
		pos = s.end
	}
	b.WriteString(line[pos:])
	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
