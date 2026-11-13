package textclean

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// JSON cleans all string values and keys without changing number precision.
// When keys collapse, an originally clean key wins, then lexical order.
func JSON(b []byte) []byte {
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return b
	}
	out, err := json.Marshal(cleanJSON(v))
	if err != nil {
		return b
	}
	return out
}
func cleanJSON(v any) any {
	switch x := v.(type) {
	case string:
		return Clean(x)
	case []any:
		for i := range x {
			x[i] = cleanJSON(x[i])
		}
		return x
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			cleanI, cleanJ := keys[i] == Clean(keys[i]), keys[j] == Clean(keys[j])
			if cleanI != cleanJ {
				return cleanI
			}
			return keys[i] < keys[j]
		})
		out := make(map[string]any, len(x))
		for _, k := range keys {
			ck := Clean(k)
			if _, exists := out[ck]; !exists {
				out[ck] = cleanJSON(x[k])
			}
		}
		return out
	}
	return v
}

// HasHiddenOnlyJSON reports whether cleaning would silently replace a nonempty
// value with blank text. Explicit empty strings and null remain valid updates.
func HasHiddenOnlyJSON(b []byte) bool {
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return false
	}
	var check func(any) bool
	check = func(v any) bool {
		switch x := v.(type) {
		case string:
			return x != "" && (Clean(x) != x || strings.ContainsRune(x, 0x2800)) && Blank(x)
		case []any:
			for _, v := range x {
				if check(v) {
					return true
				}
			}
		case map[string]any:
			for _, v := range x {
				if check(v) {
					return true
				}
			}
		}
		return false
	}
	return check(v)
}

// JSONStrings cleans JSON string tokens while preserving field order and
// whitespace, for ordered output formats. JSON must already be valid.
func JSONStrings(b []byte) []byte {
	if !json.Valid(b) {
		return b
	}
	var out bytes.Buffer
	for i := 0; i < len(b); {
		if b[i] != '"' {
			out.WriteByte(b[i])
			i++
			continue
		}
		start := i
		i++
		for i < len(b) {
			if b[i] == '\\' {
				i += 2
				continue
			}
			if b[i] == '"' {
				i++
				break
			}
			i++
		}
		var s string
		if json.Unmarshal(b[start:i], &s) != nil {
			return b
		}
		if cleaned := Clean(s); cleaned != s {
			replacement, _ := json.Marshal(cleaned)
			out.Write(replacement)
		} else {
			out.Write(b[start:i])
		}
	}
	return out.Bytes()
}
