package authz

import (
	"strings"
	"testing"
)

func TestLimitedSecretShape(t *testing.T) {
	s := NewLimitedSecret("answer")
	if !strings.HasPrefix(s, "potluck_answer_") || len(s) != len("potluck_answer_")+52 || !LooksLikeToken(s) {
		t.Fatalf("secret %q", s)
	}
	if l, ok := TokenLimit(s); !ok || l != "answer" {
		t.Fatalf("TokenLimit(%q) = %q %v", s, l, ok)
	}
	if HashLimited("answer", s) == HashToken(s) || HashLimited("answer", s) == HashLimited("other", s) || len(HashLimited("answer", s)) != 64 {
		t.Fatal("the limited hash is not separated")
	}
	plain := NewTokenSecret()
	if _, ok := TokenLimit(plain); ok {
		t.Fatalf("a plain token read as limited: %q", plain)
	}
	if _, ok := TokenLimit("zm_" + strings.TrimPrefix(plain, "potluck_")); ok {
		t.Fatal("an old token read as limited")
	}
	body := s[len(s)-52:]
	for _, bad := range []string{
		"potluck__" + body,                  // no limit
		"potluck_Answer_" + body,            // not lowercase
		"potluck_an_swer_" + body,           // underscore in the limit
		"potluck_answer-" + body,            // no separator
		"potluck_answer_" + body[:51],       // short
		"potluck_answer_" + body + "a",      // long: the limit would be "answer_a..."
		"potluck_answer_" + body[:51] + "1", // not base32
		"zm_answer_" + body,                 // old prefix
		"potluck_" + strings.Repeat("a", 17) + "_" + body,
	} {
		if _, ok := TokenLimit(bad); ok {
			t.Errorf("TokenLimit(%q) accepted", bad)
		}
	}
}

func TestValidLimit(t *testing.T) {
	for _, ok := range []string{"a", "answer", strings.Repeat("z", MaxLimitLen)} {
		if !ValidLimit(ok) {
			t.Errorf("ValidLimit(%q)", ok)
		}
	}
	for _, bad := range []string{"", "*", "an_swer", "Answer", "answer1", "ans wer", strings.Repeat("z", MaxLimitLen+1)} {
		if ValidLimit(bad) {
			t.Errorf("ValidLimit(%q) accepted", bad)
		}
	}
}

func TestSubjectLimit(t *testing.T) {
	var none *Subject
	if none.Limit() != "" || (&Subject{}).Limit() != "" || (&Subject{Cap: &Cap{Role: Owner}}).Limit() != "" {
		t.Fatal("no limit")
	}
	if (&Subject{Cap: &Cap{Limit: "answer"}}).Limit() != "answer" {
		t.Fatal("limit")
	}
}
