package client

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
)

// recorder is a minimal in-process ResponseWriter for WithHandler (no
// httptest import in production code). Streaming responses are buffered.
type recorder struct {
	code int
	hdr  http.Header
	buf  bytes.Buffer
}

func newRecorder() *recorder { return &recorder{code: 200, hdr: http.Header{}} }

func (r *recorder) Header() http.Header         { return r.hdr }
func (r *recorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Flush()                      {}

func (r *recorder) result(req *http.Request) *http.Response {
	return &http.Response{
		Status: strconv.Itoa(r.code) + " " + http.StatusText(r.code), StatusCode: r.code,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: r.hdr,
		Body: io.NopCloser(bytes.NewReader(r.buf.Bytes())), ContentLength: int64(r.buf.Len()), Request: req,
	}
}
