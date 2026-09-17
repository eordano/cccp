package cheka

import (
	"math"
	"regexp"
	"sort"
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
	hints   []string
	ci      bool
	before  int
	after   int
}

const (
	windowKey  = 8192
	windowJWT  = 16384
	windowKV   = 512
	hexRunMin  = 65
	smallLine  = 4096
	mergeSlack = 64
)

type span struct{ start, end int }

func hintAt(hay, h string, ci bool, lower *string, line string) func(from int) int {
	return func(from int) int {
		if from >= len(hay) {
			return -1
		}
		src := hay
		if ci {
			if *lower == "" {
				*lower = strings.ToLower(line)
			}
			src = *lower
		}
		i := strings.Index(src[from:], h)
		if i < 0 {
			return -1
		}
		return from + i
	}
}

func hexRuns(line string) []span {
	var out []span
	run := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			run++
			continue
		}
		if run >= hexRunMin {
			out = append(out, span{i - run, i})
		}
		run = 0
	}
	if run >= hexRunMin {
		out = append(out, span{len(line) - run, len(line)})
	}
	return out
}

func windows(line string, lower *string, p pattern) []span {
	if len(line) <= smallLine || (len(p.hints) == 0 && p.kind != "hex_token") {
		return []span{{0, len(line)}}
	}
	var ws []span
	if p.kind == "hex_token" {
		ws = hexRuns(line)
	} else {
		for _, h := range p.hints {
			next := hintAt(line, h, p.ci, lower, line)
			for i := next(0); i >= 0; i = next(i + 1) {
				a, b := i-p.before, i+len(h)+p.after
				if a < 0 {
					a = 0
				}
				if b > len(line) {
					b = len(line)
				}
				ws = append(ws, span{a, b})
			}
		}
	}
	if len(ws) < 2 {
		return ws
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].start < ws[j].start })
	merged := ws[:1]
	for _, w := range ws[1:] {
		last := &merged[len(merged)-1]
		if w.start <= last.end+mergeSlack {
			if w.end > last.end {
				last.end = w.end
			}
			continue
		}
		merged = append(merged, w)
	}
	return merged
}

func matches(line string, lower *string, p pattern) [][]int {
	var out [][]int
	for _, w := range windows(line, lower, p) {
		for _, loc := range p.re.FindAllStringSubmatchIndex(line[w.start:w.end], -1) {
			adj := make([]int, len(loc))
			for k, v := range loc {
				if v >= 0 {
					v += w.start
				}
				adj[k] = v
			}
			out = append(out, adj)
		}
	}
	return out
}

var patterns = []pattern{

	{"anthropic_key", regexp.MustCompile(`sk-ant-(?:api|admin)\d+-[A-Za-z0-9_\-]{80,}`), false, []string{"sk-ant-"}, false, 0, 8192},

	{"openai_key", regexp.MustCompile(`sk-(?:proj|svcacct|admin|None)[-_][A-Za-z0-9]{40,}`), false, []string{"sk-"}, false, 0, 8192},

	{"openai_legacy_key", regexp.MustCompile(`sk-[A-Za-z0-9]{40,}`), false, []string{"sk-"}, false, 0, 8192},

	{"github_classic_token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36}`), false, []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, false, 0, 64},

	{"github_fine_grained_pat", regexp.MustCompile(`github_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{59}`), false, []string{"github_pat_"}, false, 0, 128},

	{"gitlab_token", regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{20,}`), false, []string{"glpat-"}, false, 0, 8192},

	{"slack_webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`), false, []string{"hooks.slack.com"}, false, 16, 8192},
	{"slack_token", regexp.MustCompile(`xox[baporsz]-\d+-\d+-[A-Za-z0-9\-]+`), false, []string{"xox"}, false, 0, 8192},

	{"stripe_key", regexp.MustCompile(`(?:sk|rk|pk)_(?:live|test)_[0-9a-zA-Z]{24,}`), false, []string{"_live_", "_test_"}, false, 4, 8192},

	{"google_api_key", regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`), false, []string{"AIza"}, false, 0, 64},

	{"google_oauth_token", regexp.MustCompile(`ya29\.[0-9A-Za-z_\-]{20,}`), false, []string{"ya29."}, false, 0, 8192},

	{"huggingface_key", regexp.MustCompile(`hf_[A-Za-z0-9]{30,}`), false, []string{"hf_"}, false, 0, 8192},

	{"npm_token", regexp.MustCompile(`npm_[A-Za-z0-9]{36}`), false, []string{"npm_"}, false, 0, 64},

	{"sendgrid_key", regexp.MustCompile(`SG\.[A-Za-z0-9_\-]{22}\.[A-Za-z0-9_\-]{43}`), false, []string{"SG."}, false, 0, 128},

	{"aws_access_key", regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), false, []string{"AKIA", "ASIA"}, false, 0, 32},

	{"docker_hub_token", regexp.MustCompile(`dckr_pat_[A-Za-z0-9_\-]{20,}`), false, []string{"dckr_pat_"}, false, 0, 8192},

	{"digitalocean_token", regexp.MustCompile(`dop_v1_[a-f0-9]{64}`), false, []string{"dop_v1_"}, false, 0, 128},

	{"hasura_admin_secret", regexp.MustCompile(`hskey-auth-[a-zA-Z0-9]{20,}`), false, []string{"hskey-auth-"}, false, 0, 8192},

	{"synadia_token", regexp.MustCompile(`syt_[A-Za-z0-9_\-]{20,}`), false, []string{"syt_"}, false, 0, 8192},

	{"private_key", regexp.MustCompile(`-----BEGIN\s+(?:RSA\s+|EC\s+|OPENSSH\s+|DSA\s+)?PRIVATE\s+KEY-----`), false, []string{"-----BEGIN"}, false, 0, 128},

	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{20,}\.eyJ[A-Za-z0-9_\-]{20,}\.[A-Za-z0-9_\-]{20,}`), false, []string{"eyJ"}, false, 0, 16384},

	{"connection_string", regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^:@\s/]{1,64}:([^@\s/]{1,256})@[^\s]{1,256}`), true, []string{"://"}, false, 80, 600},

	{"bearer", regexp.MustCompile(`(?i)Bearer\s+([A-Za-z0-9_\-.]{20,})`), true, []string{"bearer"}, true, 0, 512},

	{"key_value", regexp.MustCompile(`(?i)(?:PASSWORD|PASSWD|TOKEN|SECRET|API[_ ]?KEY|ACCESS[_ ]?KEY|PRIVATE[_ ]?KEY|AUTH[_ ]?TOKEN)\s*[=:]\s*["']?([^\s"'\n]{8,})`), true, []string{"password", "passwd", "token", "secret", "api_key", "api key", "apikey", "access_key", "access key", "accesskey", "private_key", "private key", "privatekey", "auth_token", "auth token", "authtoken"}, true, 0, 512},

	{"hex_token", regexp.MustCompile(`(?i)[0-9a-f]{65,}`), true, nil, true, 0, 0},
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
	var claimed []span
	overlaps := func(a span) bool {
		for _, b := range claimed {
			if a.start < b.end && b.start < a.end {
				return true
			}
		}
		return false
	}
	var lower string
	var out []Finding
	for _, p := range patterns {
		for _, loc := range matches(line, &lower, p) {
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
	type kspan struct {
		start, end int
		kind       string
	}
	var spans []kspan
	var lower string
	for _, p := range patterns {
		for _, loc := range matches(line, &lower, p) {
			matched := line[loc[0]:loc[1]]
			evalText := matched
			if len(loc) >= 4 && loc[2] >= 0 {
				evalText = line[loc[2]:loc[3]]
			}
			if isBenign(evalText, matched, !p.generic) {
				continue
			}
			spans = append(spans, kspan{loc[0], loc[1], p.kind})
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
