package pravda

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
)

const (
	cookieHidden    = "cw_hidden"
	cookieCollapsed = "cw_collapsed"
	cookieJumpLines = "cw_jump_lines"
	cookieJumpFile  = "cw_jump_file"
)

var defaultHidden = []string{
	"thinking",
	"turn_duration",
	"permission-mode",
	"meta",
	"attachment:task_reminder",
	"attachment:deferred_tools_delta",
	"attachment:skill_listing",
	"attachment:queued_command",
	"attachment:edited_text_file",
	"queue-operation",
}

func readStringList(r *http.Request, name string) []string {
	c, err := r.Cookie(name)
	if err != nil {
		return nil
	}
	raw, err := url.QueryUnescape(c.Value)
	if err != nil {
		raw = c.Value
	}
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func writeStringList(w http.ResponseWriter, name string, list []string) {
	b, _ := json.Marshal(list)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    url.QueryEscape(string(b)),
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 365,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

func readHidden(r *http.Request) []string {
	if c, err := r.Cookie(cookieHidden); err == nil {
		raw, err := url.QueryUnescape(c.Value)
		if err != nil {
			raw = c.Value
		}
		var out []string
		if err := json.Unmarshal([]byte(raw), &out); err == nil {
			return out
		}
	}

	out := make([]string, len(defaultHidden))
	copy(out, defaultHidden)
	return out
}

func readBool(r *http.Request, name string, def bool) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return def
	}
	return c.Value == "1"
}

func writeBool(w http.ResponseWriter, name string, v bool) {
	val := "0"
	if v {
		val = "1"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    val,
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 365,
		SameSite: http.SameSiteLaxMode,
	})
}

func toggleInList(list []string, v string) []string {
	out := make([]string, 0, len(list))
	found := false
	for _, x := range list {
		if x == v {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
