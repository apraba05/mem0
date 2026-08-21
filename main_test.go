package main

import (
	"reflect"
	"testing"
)

func TestRankFactsKeywordHits(t *testing.T) {
	facts := []string{
		"Alex is vegetarian.",
		"Alex is allergic to peanuts.",
		"Alex has about 30 minutes to cook on weeknights.",
	}
	got := rankFacts(facts, "dinner without peanuts", 2)
	want := []string{"Alex is allergic to peanuts."}
	// "peanuts" matches second fact; vegetarian/30-min may not match "dinner"/"without".
	if len(got) < 1 || got[0] != want[0] {
		t.Fatalf("got %#v, want first fact about peanuts", got)
	}
}

func TestRankFactsFallbackRecent(t *testing.T) {
	facts := []string{"fact-a", "fact-b", "fact-c"}
	got := rankFacts(facts, "zzzz-no-overlap", 2)
	want := []string{"fact-b", "fact-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestTokensSkipsShort(t *testing.T) {
	tok := tokens("I am an AI fan of tofu")
	if tok["am"] || tok["an"] || tok["of"] {
		t.Fatalf("short tokens should be skipped: %#v", tok)
	}
	if !tok["tofu"] || !tok["fan"] {
		t.Fatalf("expected tofu/fan: %#v", tok)
	}
}
