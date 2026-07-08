package analysis

import (
	"reflect"
	"testing"
)

func TestAnalyze(t *testing.T) {
	got := Terms("Caffè Espresso, 100% Arabica!")
	want := []string{"caffe", "espresso", "100", "arabica"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Terms = %v, want %v", got, want)
	}
}

func TestAnalyzePositions(t *testing.T) {
	toks := Analyze("uno due tre")
	if len(toks) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(toks))
	}
	for i, tok := range toks {
		if tok.Pos != i {
			t.Errorf("token %q pos = %d, want %d", tok.Term, tok.Pos, i)
		}
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("PERÙ"); got != "peru" {
		t.Fatalf("Normalize = %q", got)
	}
}
