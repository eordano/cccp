package api

import "testing"

func TestUnifiedDiff(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"", "x\n", "--- a\n+++ b\n@@ -0,0 +1,1 @@\n+x\n"},
		{"a\nb\nc\n", "a\nB\nc\n", "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"},
		{"same\n", "same\n", ""},
		{"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", "1\n2\n3\n4\n5\n6\n7\n8\n9\nten\n", "--- a\n+++ b\n@@ -7,4 +7,4 @@\n 7\n 8\n 9\n-10\n+ten\n"},
	} {
		if got := unifiedDiff("a", "b", tc.a, tc.b); got != tc.want {
			t.Errorf("diff(%q,%q) =\n%s\nwant\n%s", tc.a, tc.b, got, tc.want)
		}
	}
}
