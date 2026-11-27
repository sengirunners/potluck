package api

import "errors"

// plainError is err with its plain form (Error.Plain), for a command's
// report to put in its --json error. Everything else about it is err's own.
type plainError struct {
	err   error
	words string
}

func (p *plainError) Error() string { return p.err.Error() }
func (p *plainError) Unwrap() error { return p.err }

// Plainly is err with its words for someone who has never used a terminal.
// The closest to the cause wins: an err that has some keeps them.
func Plainly(err error, words string) error {
	if err == nil || words == "" || PlainForm(err) != "" {
		return err
	}
	return &plainError{err: err, words: words}
}

// PlainForm is err's plain form, "" when it has none.
func PlainForm(err error) string {
	var p *plainError
	if errors.As(err, &p) {
		return p.words
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Plain
	}
	return ""
}
