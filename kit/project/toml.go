package project

// A small TOML subset, enough for project.toml and .zm/project.toml, so the
// core needs no TOML dependency:
//
//	# comments
//	key = "basic string" | 'literal' | 42 | true | ["a", "b"] (may span lines) | { k = "v" }
//	[table]
//	[[array_of_tables]]
//
// Dotted keys and nested table headers are not supported and are errors.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tomlDoc is a decoded document: root keys, plus tables and arrays of tables by name.
type tomlDoc = map[string]any

// DecodeTOML decodes src: strings, int64, bool, []any, map[string]any
// (inline tables and [table]s) and []map[string]any ([[tables]]).
func DecodeTOML(src string) (tomlDoc, error) {
	root := tomlDoc{}
	cur := root
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		ln := i + 1
		line := strings.TrimSpace(stripComment(lines[i]))
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "[["):
			if !strings.HasSuffix(line, "]]") {
				return nil, fmt.Errorf("line %d: bad table header %q", ln, line)
			}
			name := strings.TrimSpace(line[2 : len(line)-2])
			if !bareKey(name) {
				return nil, fmt.Errorf("line %d: unsupported table name %q", ln, name)
			}
			arr, _ := root[name].([]map[string]any)
			if _, exists := root[name]; exists && arr == nil {
				return nil, fmt.Errorf("line %d: %q is not an array of tables", ln, name)
			}
			t := map[string]any{}
			root[name] = append(arr, t)
			cur = t
		case strings.HasPrefix(line, "["):
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: bad table header %q", ln, line)
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if !bareKey(name) {
				return nil, fmt.Errorf("line %d: unsupported table name %q", ln, name)
			}
			if _, exists := root[name]; exists {
				return nil, fmt.Errorf("line %d: table %q defined twice", ln, name)
			}
			t := map[string]any{}
			root[name] = t
			cur = t
		default:
			key, rest, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("line %d: want key = value, got %q", ln, line)
			}
			k, err := parseKey(strings.TrimSpace(key))
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", ln, err)
			}
			val := strings.TrimSpace(rest)
			// A multi-line array: keep reading until brackets balance.
			for strings.HasPrefix(val, "[") && depth(val) > 0 && i+1 < len(lines) {
				i++
				val += " " + strings.TrimSpace(stripComment(lines[i]))
			}
			v, tail, err := parseValue(val)
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", ln, err)
			}
			if strings.TrimSpace(tail) != "" {
				return nil, fmt.Errorf("line %d: unexpected %q after value", ln, tail)
			}
			if _, dup := cur[k]; dup {
				return nil, fmt.Errorf("line %d: key %q set twice", ln, k)
			}
			cur[k] = v
		}
	}
	return root, nil
}

func bareKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func parseKey(s string) (string, error) {
	if bareKey(s) {
		return s, nil
	}
	if strings.HasPrefix(s, `"`) {
		v, tail, err := parseBasic(s)
		if err == nil && strings.TrimSpace(tail) == "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("unsupported key %q", s)
}

// stripComment removes a # comment that is not inside a string.
func stripComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q == '"' && c == '\\':
			i++
		case q != 0 && c == q:
			q = 0
		case q == 0 && (c == '"' || c == '\''):
			q = c
		case q == 0 && c == '#':
			return s[:i]
		}
	}
	return s
}

// depth is the bracket depth at the end of s, outside strings.
func depth(s string) int {
	d := 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q == '"' && c == '\\':
			i++
		case q != 0 && c == q:
			q = 0
		case q == 0 && (c == '"' || c == '\''):
			q = c
		case q == 0 && (c == '[' || c == '{'):
			d++
		case q == 0 && (c == ']' || c == '}'):
			d--
		}
	}
	return d
}

func parseValue(s string) (any, string, error) {
	s = strings.TrimLeft(s, " \t")
	switch {
	case s == "":
		return nil, "", fmt.Errorf("missing value")
	case s[0] == '"':
		return parseBasic(s)
	case s[0] == '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return nil, "", fmt.Errorf("unterminated literal string")
		}
		return s[1 : 1+end], s[2+end:], nil
	case s[0] == '[':
		var out []any
		rest := strings.TrimLeft(s[1:], " \t")
		for {
			rest = strings.TrimLeft(rest, " \t")
			if strings.HasPrefix(rest, "]") {
				return out, rest[1:], nil
			}
			v, tail, err := parseValue(rest)
			if err != nil {
				return nil, "", err
			}
			out = append(out, v)
			tail = strings.TrimLeft(tail, " \t")
			switch {
			case strings.HasPrefix(tail, ","):
				rest = tail[1:]
			case strings.HasPrefix(tail, "]"):
				return out, tail[1:], nil
			default:
				return nil, "", fmt.Errorf("want , or ] in array, got %q", tail)
			}
		}
	case s[0] == '{':
		out := map[string]any{}
		rest := strings.TrimLeft(s[1:], " \t")
		if strings.HasPrefix(rest, "}") {
			return out, rest[1:], nil
		}
		for {
			k, tail, ok := strings.Cut(rest, "=")
			if !ok {
				return nil, "", fmt.Errorf("want key = value in inline table")
			}
			key, err := parseKey(strings.TrimSpace(k))
			if err != nil {
				return nil, "", err
			}
			v, tail, err := parseValue(tail)
			if err != nil {
				return nil, "", err
			}
			out[key] = v
			tail = strings.TrimLeft(tail, " \t")
			switch {
			case strings.HasPrefix(tail, ","):
				rest = strings.TrimLeft(tail[1:], " \t")
			case strings.HasPrefix(tail, "}"):
				return out, tail[1:], nil
			default:
				return nil, "", fmt.Errorf("want , or } in inline table, got %q", tail)
			}
		}
	case strings.HasPrefix(s, "true"):
		return true, s[4:], nil
	case strings.HasPrefix(s, "false"):
		return false, s[5:], nil
	default:
		end := strings.IndexAny(s, " \t,]}")
		if end < 0 {
			end = len(s)
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(s[:end], "_", ""), 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("unsupported value %q", s[:end])
		}
		return n, s[end:], nil
	}
}

func parseBasic(s string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			return b.String(), s[i+1:], nil
		case '\\':
			if i+1 >= len(s) {
				return "", "", fmt.Errorf("bad escape")
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\':
				b.WriteByte(s[i])
			case 'u':
				if i+4 >= len(s) {
					return "", "", fmt.Errorf("bad \\u escape")
				}
				n, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
				if err != nil {
					return "", "", fmt.Errorf("bad \\u escape")
				}
				b.WriteRune(rune(n))
				i += 4
			default:
				return "", "", fmt.Errorf("unsupported escape \\%c", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", fmt.Errorf("unterminated string")
}

// TOMLString quotes s as a basic string.
func TOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == utf8.RuneError:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TOMLStrings is xs as an array of basic strings.
func TOMLStrings(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = TOMLString(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// TOMLKey is k as a key: bare when it can be, else quoted.
func TOMLKey(k string) string {
	if bareKey(k) {
		return k
	}
	return TOMLString(k)
}

// tomlValue encodes a decoded value (DecodeTOML's types), maps with sorted
// keys.
func tomlValue(v any) string {
	switch x := v.(type) {
	case string:
		return TOMLString(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	case []any:
		q := make([]string, len(x))
		for i, e := range x {
			q[i] = tomlValue(e)
		}
		return "[" + strings.Join(q, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = TOMLKey(k) + " = " + tomlValue(x[k])
		}
		if len(parts) == 0 {
			return "{}"
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return `""`
}

// tomlTable is one table's keys, sorted, one per line.
func tomlTable(t map[string]any) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(TOMLKey(k) + " = " + tomlValue(t[k]) + "\n")
	}
	return b.String()
}

// TOMLReader reads one table's fields, reporting wrong types and unknown
// keys (Done); a Section's Parse uses it too.
type TOMLReader struct {
	where string
	m     map[string]any
	seen  map[string]bool
	err   error
}

// NewTOMLReader reads m; where names it in errors.
func NewTOMLReader(where string, m map[string]any) *TOMLReader {
	return &TOMLReader{where: where, m: m, seen: map[string]bool{}}
}

// Fail records the first error.
func (r *TOMLReader) Fail(format string, a ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(r.where+": "+format, a...)
	}
}

func (r *TOMLReader) Str(k string) string {
	r.seen[k] = true
	v, ok := r.m[k]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		r.Fail("%s must be a string", k)
	}
	return s
}

func (r *TOMLReader) Strs(k string) []string {
	r.seen[k] = true
	v, ok := r.m[k]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		r.Fail("%s must be an array of strings", k)
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			r.Fail("%s must be an array of strings", k)
			return nil
		}
		out = append(out, s)
	}
	return out
}

func (r *TOMLReader) StrMap(k string) map[string]string {
	r.seen[k] = true
	v, ok := r.m[k]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		r.Fail("%s must be an inline table of strings", k)
		return nil
	}
	out := map[string]string{}
	for kk, vv := range m {
		s, ok := vv.(string)
		if !ok {
			r.Fail("%s.%s must be a string", k, kk)
			return nil
		}
		out[kk] = s
	}
	return out
}

func (r *TOMLReader) Tables(k string) []map[string]any {
	r.seen[k] = true
	v, ok := r.m[k]
	if !ok {
		return nil
	}
	ts, ok := v.([]map[string]any)
	if !ok {
		r.Fail("%s must be [[%s]] tables", k, k)
	}
	return ts
}

// Mark counts k as read; Seen reports whether it was.
func (r *TOMLReader) Mark(k string)      { r.seen[k] = true }
func (r *TOMLReader) Seen(k string) bool { return r.seen[k] }

// Bool reads a boolean (false when absent).
func (r *TOMLReader) Bool(k string) bool {
	r.seen[k] = true
	v, ok := r.m[k]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		r.Fail("%s must be true or false", k)
	}
	return b
}

// Int reads an integer (0 when absent).
func (r *TOMLReader) Int(k string) int64 {
	r.seen[k] = true
	v, ok := r.m[k]
	if !ok {
		return 0
	}
	n, ok := v.(int64)
	if !ok {
		r.Fail("%s must be an integer", k)
	}
	return n
}

// Done is the first error, or the unknown keys.
func (r *TOMLReader) Done() error {
	if r.err != nil {
		return r.err
	}
	var unknown []string
	for k := range r.m {
		if !r.seen[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: unknown keys %s", r.where, strings.Join(unknown, ", "))
	}
	return nil
}
