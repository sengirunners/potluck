// Package textclean removes hidden characters from human-readable text.
// Character ranges and emoji handling are adapted from T3 Code; see T3-LICENSE.
package textclean

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n")

// Clean removes invisible controls, retaining script shaping and emoji sequences.
func Clean(s string) string {
	s = lineBreaks.Replace(s)
	rs := []rune(s)
	out := make([]rune, 0, len(rs))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == 0x1f3f4 && i+6 < len(rs) && subdivision(rs[i+1:i+7]) {
			out = append(out, rs[i:i+7]...)
			i += 6
			continue
		}
		if r == 0xfe0e || r == 0xfe0f {
			if i > 0 && len(out) > 0 && out[len(out)-1] == rs[i-1] && variationBase(rs[i-1]) && (!keycapBase(rs[i-1]) || r == 0xfe0f && i+1 < len(rs) && rs[i+1] == 0x20e3) {
				out = append(out, r)
			}
			continue
		}
		if r != 0x200c && r != 0x200d && hidden(r) {
			continue
		}
		out = append(out, r)
	}
	joins := emojiJoiners(out)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range out {
		if r == 0x200c || r == 0x200d {
			left, right := i-1, i+1
			validScript := left >= 0 && (isVirama(out[left]) || right < len(out) && scriptPart(out[left]) && scriptPart(out[right]))
			if !validScript && !joins[i] {
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Blank reports whether text contains anything visible after cleaning.
func Blank(s string) bool {
	return strings.TrimFunc(Clean(s), func(r rune) bool { return unicode.IsSpace(r) || r == 0x2800 || r == 0x3164 }) == ""
}

func subdivision(rs []rune) bool {
	return string(rs) == "\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f" || string(rs) == "\U000e0067\U000e0062\U000e0073\U000e0063\U000e0074\U000e007f" || string(rs) == "\U000e0067\U000e0062\U000e0077\U000e006c\U000e0073\U000e007f"
}
func emoji(r rune) bool { return r >= 0x1f000 && r <= 0x1faff || r >= 0x2190 && r <= 0x2bff }
func scriptPart(r rune) bool {
	return (unicode.IsLetter(r) || unicode.IsMark(r)) && (r >= 0x600 && r <= 0x8ff || r >= 0x900 && r <= 0xdff || r >= 0x1000 && r <= 0x109f || r >= 0x1780 && r <= 0x17ff || r >= 0xa800 && r <= 0xabff || r >= 0x11000 && r <= 0x11fff)
}
func isVirama(r rune) bool   { return scriptPart(r) && norm.NFD.PropertiesString(string(r)).CCC() == 9 }
func keycapBase(r rune) bool { return r == '#' || r == '*' || r >= '0' && r <= '9' }
func hidden(r rune) bool {
	return (r < 0x600 || r > 0x605) && r != 0x6dd && (unicode.Is(unicode.Cf, r) || unicode.Is(defaultIgnorable, r)) || r <= 8 || r == 0xb || r == 0xc || r >= 0xe && r <= 0x1f || r >= 0x7f && r <= 0x9f || nonchar(r)
}

// nonchar reports Unicode's 66 noncharacters: reserved for a program's
// internal use, never text. Private-use code points stay: Nerd Font icons and
// U+F8FF render in the fonts that define them.
func nonchar(r rune) bool { return r >= 0xfdd0 && r <= 0xfdef || r&0xfffe == 0xfffe }
