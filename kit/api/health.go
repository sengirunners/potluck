package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
)

var healthKeys = sync.OnceValue(func() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeFor[Health]()
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys[name] = true
	}
	return keys
})

// HealthKeyTaken reports whether key is the JSON name of a Health field the
// base fills itself: a module's health part may not use it.
func HealthKeyTaken(key string) bool { return healthKeys()[key] }

// MarshalJSON is Health's fields, then each of Parts at the top level, in
// key order.
func (h Health) MarshalJSON() ([]byte, error) {
	type plain Health
	b, err := json.Marshal(plain(h))
	if err != nil || len(h.Parts) == 0 {
		return b, err
	}
	keys := make([]string, 0, len(h.Parts))
	for k := range h.Parts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.Write(b[:len(b)-1]) // without its closing brace
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(h.Parts[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
