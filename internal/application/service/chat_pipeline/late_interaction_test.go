package chatpipeline

import (
	"testing"
)

func TestTokenAffinity(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		min  float64
		max  float64
	}{
		{"identical", "optim", "optim", 1.0, 1.0},
		{"prefix", "optim", "optimization", 0.7, 1.0},
		{"substring", "raga", "xxragaxx", 0.5, 0.81},
		{"disjoint", "alpha", "omega", 0.0, 0.0},
		{"empty", "", "x", 0.0, 0.0},
	}
	for _, c := range cases {
		got := tokenAffinity(c.a, c.b)
		if got < c.min || got > c.max {
			t.Fatalf("%s: tokenAffinity(%q, %q) = %v, want in [%v, %v]",
				c.name, c.a, c.b, got, c.min, c.max)
		}
	}
}

func TestLateInteractionScore(t *testing.T) {
	// Exact full coverage scores 1.0.
	if got := lateInteractionScore("alpha beta", "alpha beta gamma"); got != 1.0 {
		t.Fatalf("full exact coverage = %v, want 1.0", got)
	}
	// No overlap scores 0.
	if got := lateInteractionScore("alpha", "omega psi"); got != 0 {
		t.Fatalf("disjoint score = %v, want 0", got)
	}
	// Partial overlap is normalized by query length: one of two query terms
	// matches exactly, the other matches nothing.
	got := lateInteractionScore("alpha zzz", "alpha beta")
	if got < 0.49 || got > 0.51 {
		t.Fatalf("half coverage = %v, want ~0.5", got)
	}
	// Empty inputs never panic and score 0.
	if got := lateInteractionScore("", "alpha"); got != 0 {
		t.Fatalf("empty query = %v, want 0", got)
	}
	if got := lateInteractionScore("alpha", ""); got != 0 {
		t.Fatalf("empty passage = %v, want 0", got)
	}
}

func TestLateInteractionScorePrefixBeatsNothing(t *testing.T) {
	// "optim" vs "optimization" is a prefix match: the score must land between
	// the substring floor and exact match.
	got := lateInteractionScore("optim", "optimization techniques")
	if got <= 0.5 || got >= 1.0 {
		t.Fatalf("prefix-only score = %v, want in (0.5, 1.0)", got)
	}
}
