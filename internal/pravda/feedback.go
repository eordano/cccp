package pravda

import (
	"context"
	"net/http"
	"strconv"

	"github.com/eordano/cccp/internal/sae"
)

func (s *WatchServer) decorateVotes(ctx context.Context, rel string, sess *Session) {
	if s.Sae == nil || sess == nil {
		return
	}
	votes, err := s.Sae.VotesByPath(ctx, rel)
	if err != nil {
		return
	}
	for i := range sess.Entries {
		e := &sess.Entries[i]
		if saeKind(e) == "" {
			continue
		}
		e.Votable = true
		e.Vote = votes[e.Index]
	}
}

func (s *WatchServer) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if s.Sae == nil {
		http.Error(w, "feedback needs the store", http.StatusNotFound)
		return
	}
	rel := r.PathValue("path")
	entry, _ := strconv.Atoi(r.FormValue("entry"))
	vote, _ := strconv.Atoi(r.FormValue("vote"))
	if entry <= 0 || vote < -1 || vote > 1 {
		http.Error(w, "entry and vote (-1, 0, 1) required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	ver := s.pathVersion(rel)
	sess, ok := s.parsed.getCachedSession(rel, ver)
	if !ok {
		events, err := s.events(ctx, rel, ver)
		if err != nil {
			sessionError(w, r, err)
			return
		}
		sess = Normalize(events, nil)
	}
	var target *Entry
	for i := range sess.Entries {
		if sess.Entries[i].Index == entry {
			target = &sess.Entries[i]
			break
		}
	}
	kind := ""
	if target != nil {
		kind = saeKind(target)
	}
	if kind == "" {
		http.Error(w, "entry is not a user or assistant message", http.StatusBadRequest)
		return
	}
	f := sae.Feedback{Path: rel, Entry: entry, RecordID: sae.RecordID(kind, target.Text), Kind: kind, Vote: vote, Note: r.FormValue("note"), Who: r.FormValue("who")}
	if err := s.Sae.Vote(ctx, f); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"vote":` + strconv.Itoa(vote) + `}` + "\n"))
}
