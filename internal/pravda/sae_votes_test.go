package pravda

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eordano/cccp/internal/sae"
)

func saeVoteFixture(t *testing.T) *WatchServer {
	t.Helper()
	s, db := saeTestServer(t)
	ctx := context.Background()
	for _, r := range []struct {
		enc, id, snippet, feats string
	}{
		{"new", "r1", "first upvoted message", `[{"f":0,"p":40},{"f":1,"p":70}]`},
		{"new", "r2", "downvoted message", `[{"f":1,"p":55}]`},
		{"new", "r3", "second upvoted message", `[{"f":0,"p":90}]`},
		{"old", "r2", "downvoted message", `[{"f":0,"p":10}]`},
	} {
		if _, err := db.Exec(`INSERT INTO sae_records VALUES (?,?,'user','claude','h','s','','p.jsonl',1,?,?,0)`, r.enc, r.id, r.snippet, r.feats); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO sae_record_features SELECT ?, json_extract(value,'$.f'), ?, json_extract(value,'$.p') FROM json_each(?)`, r.enc, r.id, r.feats); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec(`INSERT INTO sae_features(encoder,feature,name) VALUES ('new',0,'Planning'),('new',1,'Debugging')`)
	for _, f := range []sae.Feedback{
		{Path: "a.jsonl", Entry: 1, RecordID: "r1", Kind: "user", Vote: 1, Note: "useful"},
		{Path: "a.jsonl", Entry: 2, RecordID: "r2", Kind: "user", Vote: -1},
		{Path: "b.jsonl", Entry: 4, RecordID: "r3", Kind: "user", Vote: 1},
	} {
		if err := s.Sae.Vote(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestSaeFeatureListShowsAndSortsVotes(t *testing.T) {
	s := saeVoteFixture(t)
	get := func(query string) string {
		w := httptest.NewRecorder()
		s.handleSaeFeatures(w, httptest.NewRequest("GET", "/sae/features?encoder=new"+query, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", query, w.Code)
		}
		return w.Body.String()
	}
	body := get("&sort=ratio")
	planning, debugging := strings.Index(body, ">Planning<"), strings.Index(body, ">Debugging<")
	if planning < 0 || debugging < 0 || planning > debugging {
		t.Fatalf("ratio sort: planning=%d debugging=%d", planning, debugging)
	}
	if !strings.Contains(body, `<td class="mono">100%</td>`) || !strings.Contains(body, `<td class="mono">50%</td>`) {
		t.Fatal("ratio column missing")
	}
	if !strings.Contains(body, `href="/sae/feature/0?encoder=new"`) {
		t.Fatal("feature drill link missing")
	}
	if !strings.Contains(body, "sort=ratio&amp;sort_dir=asc") || !strings.Contains(body, "ratio ▼") {
		t.Fatal("active column does not offer the reverse order")
	}
	body = get("&sort=down")
	if strings.Index(body, ">Debugging<") > strings.Index(body, ">Planning<") {
		t.Fatal("down sort")
	}
}

func TestSaeFeaturePageListsFeedbackForThatDictionaryOnly(t *testing.T) {
	s := saeVoteFixture(t)
	page := func(enc, id string) string {
		r := httptest.NewRequest("GET", "/sae/feature/"+id+"?encoder="+enc, nil)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		s.handleSaeFeature(w, r)
		if w.Code != 200 {
			t.Fatalf("%s/%s: %d", enc, id, w.Code)
		}
		return w.Body.String()
	}
	body := page("new", "1")
	if !strings.Contains(body, "first upvoted message") || !strings.Contains(body, "downvoted message") || strings.Contains(body, "second upvoted message") {
		t.Fatal("feature 1 feedback rows")
	}
	if !strings.Contains(body, "useful") || !strings.Contains(body, "👍1 👎1") || !strings.Contains(body, "vendor/new layer 1") || !strings.Contains(body, "/session/a.jsonl?event=2&amp;encoder=new") {
		t.Fatal("feature page detail")
	}
	body = page("old", "0")
	if !strings.Contains(body, "downvoted message") || strings.Contains(body, "upvoted") || !strings.Contains(body, "👍0 👎1") {
		t.Fatal("old dictionary must only see its own records")
	}
	if !strings.Contains(page("new", "7"), "No feedback") {
		t.Fatal("empty feature")
	}
}

func TestSaeInspectorTableSortsByVotes(t *testing.T) {
	s := saeVoteFixture(t)
	view := &saeInspectorData{Tags: []SaeTag{{Feature: 0, Pct: 20}, {Feature: 1, Pct: 80}, {Feature: 5, Pct: 99}}}
	u, _ := url.Parse("/session/a.jsonl?event=1&encoder=new&sae_sort=ratio")
	s.saeInspectorTable(context.Background(), view, "new", u)
	if view.NoVotes || view.Tags[0].Feature != 0 || view.Tags[1].Feature != 1 || view.Tags[2].Feature != 5 {
		t.Fatalf("ratio order: %+v", view.Tags)
	}
	if view.Tags[1].Votes != (sae.FeatureVotes{Up: 1, Down: 1}) {
		t.Fatalf("votes: %+v", view.Tags[1].Votes)
	}
	if h := view.Sort["ratio"]; h.Arrow != " ▼" || !strings.Contains(h.URL, "sae_sort_dir=asc") || !strings.Contains(h.URL, "event=1") {
		t.Fatalf("header: %+v", h)
	}
	u, _ = url.Parse("/session/a.jsonl?event=1&sae_sort=ratio&sae_sort_dir=asc")
	view.sortTags(u)
	if view.Tags[0].Feature != 1 || view.Tags[2].Feature != 5 {
		t.Fatalf("ascending ratio keeps unvoted last: %+v", view.Tags)
	}
	u, _ = url.Parse("/session/a.jsonl?event=1")
	view.sortTags(u)
	if view.Tags[0].Feature != 5 || view.Sort["pct"].Arrow != " ▼" {
		t.Fatalf("default activation order: %+v", view.Tags)
	}
}
