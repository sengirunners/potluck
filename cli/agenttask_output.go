package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// taskOutput remembers disk errors while output following continues, so the
// task's commits can still be collected and its box released.
type taskOutput struct {
	file    *os.File
	err     error
	dropped int64
}

func openTaskOutput(path string) (*taskOutput, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("--stdout-file: %w", err)
	}
	return &taskOutput{file: f}, nil
}

func (w *taskOutput) Write(p []byte) (int, error) {
	if w.err == nil {
		n, err := w.file.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		w.err = err
	}
	return len(p), nil
}

func (w *taskOutput) close() error {
	if err := w.file.Close(); w.err == nil {
		w.err = err
	}
	if w.dropped > 0 {
		w.err = errors.Join(w.err, fmt.Errorf("%d stdout bytes were no longer retained by the box; capture is incomplete", w.dropped))
	}
	if w.err != nil {
		return fmt.Errorf("--stdout-file: %w", w.err)
	}
	return nil
}
