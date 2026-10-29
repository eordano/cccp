package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestVersionAtFollowsHostClockAndIngest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	sha := func(v string) *string { return &v }
	for _, fv := range []FileVersion{
		{Path: "skills/x/SKILL.md", Version: 1, ContentSHA: sha("a"), MtimeNS: 100, HostID: "A", IngestedAt: 110},
		{Path: "skills/x/SKILL.md", Version: 2, ContentSHA: sha("b"), MtimeNS: 200, HostID: "B", IngestedAt: 300},
		{Path: "skills/x/SKILL.md", Version: 3, ContentSHA: nil, MtimeNS: 400, HostID: "A", IngestedAt: 405},
		{Path: "skills/y/SKILL.md", Version: 1, ContentSHA: sha("c"), MtimeNS: 50, HostID: "A", IngestedAt: 60},
	} {
		if err := s.AppendVersion(ctx, fv); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		host string
		t    int64
		want int64
	}{
		{"A", 99, 0},
		{"A", 100, 1},
		{"B", 105, 0},
		{"B", 110, 1},
		{"B", 200, 2},
		{"A", 250, 1},
		{"A", 300, 2},
		{"A", 400, 3},
		{"B", 404, 2},
	}
	for _, c := range cases {
		fv, err := s.VersionAt(ctx, "skills/x/SKILL.md", c.host, c.t)
		if err != nil {
			t.Fatal(err)
		}
		got := int64(0)
		if fv != nil {
			got = fv.Version
		}
		if got != c.want {
			t.Errorf("VersionAt(host %s, t %d) = v%d, want v%d", c.host, c.t, got, c.want)
		}
	}
	if fv, _ := s.VersionAt(ctx, "skills/x/SKILL.md", "A", 500); fv == nil || fv.ContentSHA != nil {
		t.Errorf("a tombstone in effect must come back as a deletion, got %+v", fv)
	}
	paths, err := s.VersionedPaths(ctx, "skills/")
	if err != nil || len(paths) != 2 {
		t.Errorf("VersionedPaths = %v, %v", paths, err)
	}
}

func TestVersionAtHonoursTheReviewGate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.DB().Exec(`UPDATE review_meta SET value = 1000 WHERE key = 'gate_since'`); err != nil {
		t.Fatal(err)
	}
	sha := func(v string) *string { return &v }
	const p = "memory/x.md"
	for _, fv := range []FileVersion{
		{Path: p, Version: 1, ContentSHA: sha("a"), Mode: 0o644, MtimeNS: 100, HostID: "A", IngestedAt: 110},
		{Path: p, Version: 2, ContentSHA: sha("b"), Mode: 0o644, MtimeNS: 2000, HostID: "B", IngestedAt: 2100},
		{Path: p, Version: 3, ContentSHA: sha("c"), Mode: 0o644, MtimeNS: 3900, HostID: "B", IngestedAt: 4000},
	} {
		if err := s.AppendVersion(ctx, fv); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []Review{
		{Path: p, Version: 2, Decision: DecisionApproved, DecidedAt: 3000},
		{Path: p, Version: 2, Decision: DecisionRejected, DecidedAt: 5000},
	} {
		if err := s.AddReview(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		host string
		t    int64
		want int64
	}{
		{"A", 2500, 1},
		{"B", 2050, 2},
		{"A", 3000, 2},
		{"A", 4500, 2},
		{"B", 4500, 3},
		{"A", 5000, 1},
	} {
		fv, err := s.VersionAt(ctx, p, c.host, c.t)
		if err != nil {
			t.Fatal(err)
		}
		got := int64(0)
		if fv != nil {
			got = fv.Version
		}
		if got != c.want {
			t.Errorf("VersionAt(host %s, t %d) = v%d, want v%d", c.host, c.t, got, c.want)
		}
	}
}
