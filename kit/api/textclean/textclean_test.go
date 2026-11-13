package textclean

import (
	"testing"
	"unicode"
)

func TestClean(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\u200bb\u200cc\u200dd", "abcd"},
		{"a\u061c\u200e\u200f\u202e\u2066b", "ab"},
		{"a\U000e0001\U000e0061\U000e007fb", "ab"},
		{"a\x00\u00ad\ufeffb\n\t", "ab\n\t"},
		{"👩‍💻 ❤️ 👨‍👩‍👧", "👩‍💻 ❤️ 👨‍👩‍👧"},
	} {
		if got := Clean(tc.in); got != tc.want {
			t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReviewCases(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\ufe0e\ufe0f", ""}, {"a\ufe0f\u200d\ufe0fb", "ab"}, {"🐈‍🐕", "🐈🐕"}, {"❤\u200b\ufe0f", "❤"},
		{"می\u200cخواهم", "می\u200cخواهم"}, {"ශ්‍රී", "ශ්‍රී"}, {"क्\u200dष", "क्\u200dष"}, {"ಕ್\u200cಷ", "ಕ್\u200cಷ"},
		{"🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f", "🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"},
		{"a\u2028b\u2029c\rd\r\ne", "a\nb\nc\nd\ne"},
	} {
		if got := Clean(tc.in); got != tc.want {
			t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReview2UnicodePropertiesAndEmoji(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"a\U00013430b", "ab"}, {"1\ufe0f2\ufe0e", "12"}, {"1\ufe0f\u20e3", "1\ufe0f\u20e3"}, {"1\ufe0e\u20e3", "1\u20e3"}, {"🏳‍🌈", "🏳‍🌈"}} {
		if got := Clean(tc.in); got != tc.want {
			t.Errorf("Clean(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestReview2AllFormatAndDefaultIgnorables(t *testing.T) {
	for _, table := range []*unicode.RangeTable{unicode.Cf, defaultIgnorable} {
		for _, x := range table.R16 {
			for r := rune(x.Lo); r <= rune(x.Hi); r += rune(x.Stride) {
				if r >= 0x600 && r <= 0x605 || r == 0x6dd {
					continue
				}
				if got := Clean(string(r)); got != "" {
					t.Errorf("U+%04X survived as %q", r, got)
				}
			}
		}
		for _, x := range table.R32 {
			for r := rune(x.Lo); r <= rune(x.Hi); r += rune(x.Stride) {
				if r >= 0x600 && r <= 0x605 || r == 0x6dd {
					continue
				}
				if got := Clean(string(r)); got != "" {
					t.Errorf("U+%04X survived as %q", r, got)
				}
			}
		}
	}
}

func TestReview3ShapingAndVisibleArabic(t *testing.T) {
	for _, text := range []string{"അവന്\u200d", "അവന്\u200d text", "ত্\u200d", "\u0600\u0601\u0602\u0603\u0604\u0605\u06dd"} {
		if got := Clean(text); got != text {
			t.Errorf("Clean(%q) = %q", text, got)
		}
	}
}

func TestNoncharactersGoPrivateUseStays(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\ufdd0\ufdefb", "ab"}, {"a\ufffe\uffffb", "ab"}, {"a\U0001fffe\U0010ffffb", "ab"},
		{"\ue0b0 git \uf8ff \U000f0001", "\ue0b0 git \uf8ff \U000f0001"}, {"a\ufdcf\ufdf0\ufffdb", "a\ufdcf\ufdf0\ufffdb"},
	} {
		if got := Clean(tc.in); got != tc.want {
			t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if !Blank("\ufdd0\U0010ffff") {
		t.Error("noncharacters alone should be blank")
	}
}
