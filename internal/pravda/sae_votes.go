package pravda

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/eordano/cccp/internal/sae"
)

type saeSort struct {
	Key string
	Asc bool
}

type saeSortHeader struct {
	URL   string
	Arrow string
}

func readSaeSort(q url.Values, param string, keys []string, def string) saeSort {
	key := q.Get(param)
	ok := false
	for _, k := range keys {
		ok = ok || k == key
	}
	if !ok {
		key = def
	}
	asc := key == "feature"
	switch q.Get(param + "_dir") {
	case "asc":
		asc = true
	case "desc":
		asc = false
	}
	return saeSort{Key: key, Asc: asc}
}

func saeSortHeaders(u *url.URL, param string, cur saeSort, keys []string) map[string]saeSortHeader {
	out := map[string]saeSortHeader{}
	for _, k := range keys {
		q := u.Query()
		q.Set(param, k)
		q.Del(param + "_dir")
		h := saeSortHeader{}
		if k == cur.Key {
			h.Arrow = " ▼"
			dir := "asc"
			if cur.Asc {
				h.Arrow, dir = " ▲", "desc"
			}
			q.Set(param+"_dir", dir)
		}
		h.URL = (&url.URL{Path: u.Path, RawQuery: q.Encode()}).String()
		out[k] = h
	}
	return out
}

type saeVoteKey struct {
	ID    int
	Value int
	Votes sae.FeatureVotes
}

func sortSaeRows[T any](rows []T, s saeSort, key func(T) saeVoteKey) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := key(rows[i]), key(rows[j])
		var x, y float64
		switch s.Key {
		case "feature":
			x, y = float64(a.ID), float64(b.ID)
		case "up":
			x, y = float64(a.Votes.Up), float64(b.Votes.Up)
		case "down":
			x, y = float64(a.Votes.Down), float64(b.Votes.Down)
		case "ratio":
			x, y = a.Votes.Ratio(), b.Votes.Ratio()
			if (x < 0) != (y < 0) {
				return y < 0
			}
		default:
			x, y = float64(a.Value), float64(b.Value)
		}
		if x != y {
			return (x < y) == s.Asc
		}
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		return a.ID < b.ID
	})
}

var saeFeatureSortKeys = []string{"feature", "count", "up", "down", "ratio"}
var saeInspectorSortKeys = []string{"feature", "pct", "up", "down", "ratio"}

func (s *WatchServer) saeInspectorTable(ctx context.Context, v *saeInspectorData, enc string, u *url.URL) {
	if v == nil || len(v.Tags) == 0 || s.Sae == nil {
		return
	}
	votes, _, _, err := s.Sae.FeatureVoteStats(ctx, enc)
	v.NoVotes = err != nil
	for i := range v.Tags {
		v.Tags[i].Votes = votes[v.Tags[i].Feature]
	}
	v.sortTags(u)
}

func (v *saeInspectorData) sortTags(u *url.URL) {
	s := readSaeSort(u.Query(), "sae_sort", saeInspectorSortKeys, "pct")
	sortSaeRows(v.Tags, s, func(t SaeTag) saeVoteKey { return saeVoteKey{ID: t.Feature, Value: t.Pct, Votes: t.Votes} })
	v.Sort = saeSortHeaders(u, "sae_sort", s, saeInspectorSortKeys)
}

type saeFeaturePage struct {
	Encoder  string
	Model    string
	Layer    int
	Feature  sae.Feature
	Records  int
	Votes    sae.FeatureVotes
	Feedback []sae.FeatureFeedback
}

func (s *WatchServer) handleSaeFeature(w http.ResponseWriter, r *http.Request) {
	if s.Sae == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	id, err := strconv.Atoi(r.PathValue("id"))
	enc := r.URL.Query().Get("encoder")
	if err != nil || id < 0 || !s.validEncoder(ctx, enc) {
		http.NotFound(w, r)
		return
	}
	page := saeFeaturePage{Encoder: enc, Feature: sae.Feature{Feature: id}}
	if encs, err := s.Sae.EncoderCatalog(ctx); err == nil {
		for _, e := range encs {
			if e.Encoder == enc {
				page.Model, page.Layer = e.Model, e.Layer
			}
		}
	}
	feats, err := s.Sae.Features(ctx, enc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if f, ok := feats[id]; ok {
		page.Feature = f
	}
	counts, _, err := s.Sae.FeatureCounts(ctx, enc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.Records = counts[id]
	votes, _, _, err := s.Sae.FeatureVoteStats(ctx, enc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page.Votes = votes[id]
	if page.Feedback, err = s.Sae.FeedbackForFeature(ctx, enc, id, 200); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.ExecuteTemplate(w, "sae-feature-page", page)
}
