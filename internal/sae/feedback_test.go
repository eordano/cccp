package sae

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFeatureVotesAggregatePerDictionary(t *testing.T) {
	ctx := context.Background()
	st := openDB(t)
	dir := t.TempDir()
	writeRun(t, filepath.Join(dir, "a"), "encA", 10, 8)
	writeRun(t, filepath.Join(dir, "b"), "encB", 1, 8)
	for _, d := range []string{"a", "b"} {
		if _, err := ImportWith(ctx, st.DB(), filepath.Join(dir, d), ImportOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := New(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []Feedback{
		{Path: "p1", Entry: 1, RecordID: "id000", Kind: "user", Vote: 1},
		{Path: "p2", Entry: 1, RecordID: "id000", Kind: "user", Vote: -1},
		{Path: "p3", Entry: 1, RecordID: "id000", Kind: "user", Vote: 1, Note: "good"},
		{Path: "p1", Entry: 2, RecordID: "id001", Kind: "assistant", Vote: -1},
		{Path: "p1", Entry: 3, RecordID: "id002", Kind: "user", Vote: 1},
		{Path: "p2", Entry: 3, RecordID: "id002", Kind: "user", Vote: -1},
		{Path: "p9", Entry: 1, RecordID: "id099", Kind: "user", Vote: 1},
	} {
		if err := ix.Vote(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	votes, up, down, err := ix.FeatureVoteStats(ctx, "encA")
	if err != nil || up != 1 || down != 1 {
		t.Fatalf("totals: %d %d %v", up, down, err)
	}
	want := map[int]FeatureVotes{0: {1, 0}, 1: {1, 1}, 2: {1, 1}, 3: {0, 1}}
	if len(votes) != len(want) {
		t.Fatalf("votes: %+v", votes)
	}
	for f, w := range want {
		if votes[f] != w {
			t.Fatalf("feature %d: %+v want %+v", f, votes[f], w)
		}
	}
	if votes[1].Ratio() != 0.5 || votes[1].RatioText() != "50%" || votes[4].Ratio() != -1 || votes[4].RatioText() != "—" {
		t.Fatalf("ratio: %v %q", votes[1].Ratio(), votes[4].RatioText())
	}
	votesB, upB, downB, err := ix.FeatureVoteStats(ctx, "encB")
	if err != nil || upB != 1 || downB != 0 || votesB[3] != (FeatureVotes{}) || votesB[1] != (FeatureVotes{1, 0}) {
		t.Fatalf("encB: %+v %d %d %v", votesB, upB, downB, err)
	}
	rows, err := ix.FeedbackForFeature(ctx, "encA", 1, 0)
	if err != nil || len(rows) != 4 {
		t.Fatalf("feature feedback: %+v %v", rows, err)
	}
	pct := map[string]int{}
	for _, r := range rows {
		pct[r.RecordID] = r.Pct
		if r.Snippet == "" {
			t.Fatalf("missing snippet: %+v", r)
		}
	}
	if pct["id000"] != 50 || pct["id001"] != 25 {
		t.Fatalf("activation: %v", pct)
	}
	if rows, _ := ix.FeedbackForFeature(ctx, "encB", 3, 0); len(rows) != 0 {
		t.Fatalf("encB feature 3 feedback: %+v", rows)
	}
}

func TestFeatureVotesWithoutFeedback(t *testing.T) {
	ctx := context.Background()
	st := openDB(t)
	ix, err := New(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	votes, up, down, err := ix.FeatureVoteStats(ctx, "none")
	if err != nil || len(votes) != 0 || up != 0 || down != 0 {
		t.Fatalf("%+v %d %d %v", votes, up, down, err)
	}
}
