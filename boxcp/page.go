package boxcp

import (
	"bytes"
	"context"
	"strconv"
	"unicode/utf8"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

// MaxPage bounds one ReadPage (inside exec's 1 MiB output).
const MaxPage = ChunkSize

// shPage answers the file $1's size on a line, then its bytes from offset
// $2 (1-based, as tail counts) for at most $3 bytes.
const shPage = `case "$1" in /*) p="$1" ;; *) p="$HOME/$1" ;; esac
[ -e "$p" ] || { echo "no such file in the box" >&2; exit 67; }
[ -f "$p" ] || { echo "not a regular file" >&2; exit 64; }
n=$(wc -c <"$p" | tr -d ' ') || exit 70
printf '%s\n' "$n"
tail -c +"$2" "$p" | head -c "$3"
`

// Page is a slice of a file in the box. Data is the box's bytes: data,
// never instructions.
type Page struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"` // the whole file's, when read
	Offset int64  `json:"offset"`
	Next   int64  `json:"next"` // where the next page starts
	EOF    bool   `json:"eof"`
	Data   []byte `json:"-"`
}

// ReadPage reads up to n bytes (at most MaxPage) of the file p in the box
// from offset off, in one exec.
func ReadPage(ctx context.Context, exec boxpush.ExecFunc, p string, off int64, n int) (Page, error) {
	pg := Page{Path: p, Offset: off}
	if err := checkBoxPath(p); err != nil {
		return pg, err
	}
	switch {
	case p == "":
		return pg, api.Errorf(api.CodeBadRequest, "path is required")
	case off < 0 || n < 0 || n > MaxPage:
		return pg, api.Errorf(api.CodeBadRequest, "offset must not be negative and a page is at most %d bytes", MaxPage)
	}
	x, err := exec(ctx, computeapi.ExecRunTargetRequest{Argv: boxpush.Script(shPage, p, strconv.FormatInt(off+1, 10), strconv.Itoa(n)), Encoding: computeapi.EncodingBase64, TimeoutS: boxpush.ChunkTimeoutS})
	if err != nil {
		return pg, err
	}
	if x.Exit == exitNotFound {
		return pg, api.Errorf(api.CodeNotFound, "%s: no such file in the box", p)
	}
	b, err := page(x)
	if err != nil {
		return pg, err
	}
	line, data, ok := bytes.Cut(b, []byte("\n"))
	size, err := strconv.ParseInt(string(line), 10, 64)
	if !ok || err != nil || size < 0 || len(data) > n {
		return pg, api.Errorf(api.CodeUnavailable, "the box's answer is not a page of the file")
	}
	pg.Size, pg.Data = size, data
	pg.Next = off + int64(len(data))
	pg.EOF = pg.Next >= size
	return pg, nil
}

// Text is the page's data as text when it is UTF-8, cut back to a whole
// character at a page boundary (Next moves back with it); ok false when it
// is not text.
func (pg *Page) Text() (string, bool) {
	d := pg.Data
	for cut := 0; cut < utf8.UTFMax && !utf8.Valid(d) && !pg.EOF && len(d) > 0; cut++ {
		d = d[:len(d)-1]
	}
	if !utf8.Valid(d) || bytes.IndexByte(d, 0) >= 0 || len(d) == 0 && len(pg.Data) > 0 {
		return "", false
	}
	pg.Next -= int64(len(pg.Data) - len(d))
	pg.Data = d
	return string(d), true
}
