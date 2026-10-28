package catalog

import "testing"

func TestLikePrefix(t *testing.T) {
	if _, err := likePrefix("  "); err == nil {
		t.Fatal("empty prefix was accepted")
	}
	got, err := likePrefix("fixture://urag/")
	if err != nil || got != "fixture://urag/%" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = likePrefix(`a%_b\`)
	if err != nil || got != `a\%\_b\\%` {
		t.Fatalf("%q %v", got, err)
	}
}
