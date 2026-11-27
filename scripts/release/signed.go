package main

import (
	"bytes"
	"debug/macho"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mac programs are signed on a Mac, by a job that is not the one that packs
// and signs the release. So the release builds them itself too and takes a
// signed copy only when the signature is all that differs: what the release
// key vouches for is always this build.

// buildDarwin builds the darwin targets' programs into dir/<os>_<arch>/.
func buildDarwin(version, commit, dir string, targets []string) error {
	if !versionRE.MatchString(version) {
		return fmt.Errorf("bad version %q", version)
	}
	for _, t := range targets {
		goos, goarch, err := splitTarget(t)
		if err != nil {
			return err
		}
		if goos != "darwin" {
			continue
		}
		d := filepath.Join(dir, goos+"_"+goarch)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := buildPrograms(version, commit, goos, goarch, d); err != nil {
			return err
		}
	}
	return nil
}

// takeSigned puts the signed copy of each named program in place of the built
// one in dir.
func takeSigned(signed, dir string, names []string) error {
	for _, n := range names {
		s, err := os.ReadFile(filepath.Join(signed, n))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		if err := onlySigned(b, s); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), s, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// onlySigned reports how signed differs from the Mach-O built in anything but
// its code signature: the load commands that place it (LC_CODE_SIGNATURE and
// __LINKEDIT's size) and the bytes after the program's own.
func onlySigned(built, signed []byte) error {
	b, err := readProgram(built)
	if err != nil {
		return fmt.Errorf("the built program: %w", err)
	}
	s, err := readProgram(signed)
	if err != nil {
		return fmt.Errorf("the signed program: %w", err)
	}
	if !s.signature {
		return errors.New("it carries no signature")
	}
	switch {
	case b.cpu != s.cpu || b.start != s.start || len(s.body) < len(b.body):
		return errors.New("it is not this build")
	case len(bytes.Trim(s.body[len(b.body):], "\x00")) != 0 || len(s.body)-len(b.body) >= 16: // codesign's alignment
		return errors.New("it holds more than this build")
	case !bytes.Equal(b.loads, s.loads) || !bytes.Equal(b.body, s.body[:len(b.body)]):
		return errors.New("it differs from this build in more than its signature")
	}
	return nil
}

type program struct {
	cpu       macho.Cpu
	loads     []byte // the load commands a signature leaves alone
	start     uint32 // where body starts
	body      []byte // the first section to the signature (or the end)
	signature bool
}

const lcCodeSignature = 0x1d

func readProgram(b []byte) (program, error) {
	f, err := macho.NewFile(bytes.NewReader(b))
	if err != nil {
		return program{}, err
	}
	end := uint32(len(b))
	p := program{cpu: f.Cpu, start: end}
	for _, l := range f.Loads {
		raw := l.Raw()
		if f.ByteOrder.Uint32(raw) == lcCodeSignature {
			if len(raw) < 16 {
				return program{}, errors.New("a short LC_CODE_SIGNATURE")
			}
			p.signature = true
			end = f.ByteOrder.Uint32(raw[8:])
			continue
		}
		if seg, ok := l.(*macho.Segment); ok && seg.Name == "__LINKEDIT" {
			p.loads = fmt.Appendf(p.loads, "__LINKEDIT %d %d %d %d %d %d;", seg.Addr, seg.Offset, seg.Maxprot, seg.Prot, seg.Nsect, seg.Flag)
			continue
		}
		p.loads = append(p.loads, raw...)
	}
	for _, s := range f.Sections {
		// A zero-fill section has no bytes in the file.
		if s.Offset > 0 && s.Offset < p.start && !strings.HasSuffix(s.Name, "bss") {
			p.start = s.Offset
		}
	}
	if end > uint32(len(b)) || p.start >= end {
		return program{}, errors.New("no program in it")
	}
	p.body = b[p.start:end]
	return p, nil
}
