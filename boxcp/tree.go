package boxcp

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/internal/tarsafe"
	"github.com/sengirunners/potluck/kit/api"
)

// TreeFile is one file WriteTree writes: Path relative to the tree's root.
type TreeFile struct {
	Path string
	Data []byte
}

// WriteTree writes files under the directory dir in the box (relative to its
// HOME, or absolute), merged into what is there, each a regular file of mode
// 0644 at exactly its path. It goes up as one tar, the way Upload sends a
// directory. A path that is absolute, holds a ".." or an empty segment, or is
// named twice is refused before anything is sent.
func WriteTree(ctx context.Context, exec boxpush.ExecFunc, dir string, files []TreeFile, o Options) (Result, error) {
	if err := o.defaults(); err != nil {
		return Result{}, err
	}
	if err := checkBoxPath(dir); err != nil {
		return Result{}, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	seen := map[string]bool{}
	for _, f := range files {
		name, ok := tarsafe.Name(f.Path)
		if !ok || name != f.Path || name == "." || seen[name] {
			return Result{}, api.Errorf(api.CodeBadRequest, "file path %q: a relative path, clean, once", f.Path)
		}
		seen[name] = true
		h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(f.Data)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return Result{}, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return Result{}, err
		}
		if int64(buf.Len()) > o.Max {
			return Result{}, tooLarge("the files", int64(buf.Len()), o.Max)
		}
	}
	if err := tw.Close(); err != nil {
		return Result{}, err
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	res := Result{From: "content", Kind: KindDir, Files: len(files)}
	return put(ctx, exec, bytes.NewReader(data), int64(len(data)), hex.EncodeToString(sum[:]), dir, "", 0, res, o)
}
