package teamlink_test

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// ReadBody takes a body of up to MaxForwardBody whole and refuses a larger one.
func TestReadBody(t *testing.T) {
	ok := bytes.Repeat([]byte("a"), teamlink.MaxForwardBody)
	if b, err := teamlink.ReadBody(httptest.NewRequest("POST", "/", bytes.NewReader(ok))); err != nil || len(b) != len(ok) {
		t.Fatalf("a body of MaxForwardBody: %d bytes, %v", len(b), err)
	}
	_, err := teamlink.ReadBody(httptest.NewRequest("POST", "/", bytes.NewReader(append(ok, 'b'))))
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeBadRequest || e.Message != "body over 1 MiB" || !strings.Contains(e.Hint, "at most 1 MiB") {
		t.Fatalf("a body over MaxForwardBody: %#v, want bad_request over 1 MiB", err)
	}
}
