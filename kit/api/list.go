package api

import "encoding/json"

// List is a slice that is always an array on the wire: nil marshals as []
// and null decodes as an empty list, so a reader never has to tell null from
// [] (a TypeScript screen reads both the same way only if it never sees
// null). A []T converts to it without a cast. Use it for a field that is a
// list in every answer; a list that may be absent is a pointer field.
type List[T any] []T

// MarshalJSON writes nil as [].
func (l List[T]) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]T(l))
}

// UnmarshalJSON reads null as an empty list.
func (l *List[T]) UnmarshalJSON(b []byte) error {
	var s []T
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == nil {
		s = []T{}
	}
	*l = s
	return nil
}
