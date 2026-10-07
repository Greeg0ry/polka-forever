// Package convert turns FB2 books into other e-book formats by running an
// external converter. Two command-line tools by rupor-github are supported:
//
//   - fbc  (github.com/rupor-github/fb2cng) — the maintained one: EPUB,
//     KEPUB, KFX and PDF, no further dependencies;
//   - fb2c (github.com/rupor-github/fb2converter) — its end-of-life
//     predecessor, the only one that produces MOBI and AZW3, and only with
//     Amazon's kindlegen next to it.
//
// Both are GPL-3.0 programs whose modules cannot be imported, so they are
// executed rather than linked. Polka works without them: the formats on
// offer are whatever the installed tools can produce.
package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

var (
	// ErrUnsupported is returned for a format no installed tool can produce.
	ErrUnsupported = errors.New("convert: format is not available")
	// ErrBusy is returned when too many conversions are already waiting.
	ErrBusy = errors.New("convert: too many conversions in progress")
)

const (
	// A large illustrated book takes seconds; kindlegen can take a minute.
	timeout = 3 * time.Minute
	// Conversions are CPU- and memory-hungry, so only a few run at once.
	maxParallel = 2
	// Requests beyond this many (running + waiting) are turned away at once
	// instead of piling up behind conversions that take minutes.
	maxPending = 8
)

// Format describes one output format.
type Format struct {
	Key  string // URL and API name: "epub", "mobi", …
	Ext  string // file extension without the leading dot
	Mime string
}

// formats in the order they are offered to the user.
var formats = []Format{
	{"epub", "epub", "application/epub+zip"},
	{"mobi", "mobi", "application/x-mobipocket-ebook"},
	{"azw3", "azw3", "application/vnd.amazon.ebook"},
	{"kfx", "kfx", "application/vnd.amazon.ebook"},
	{"kepub", "kepub.epub", "application/epub+zip"},
	{"pdf", "pdf", "application/pdf"},
}

// What each tool calls our formats (its --to value).
var (
	fbcTypes  = map[string]string{"epub": "epub2", "kepub": "kepub", "kfx": "kfx", "pdf": "pdf"}
	fb2cTypes = map[string]string{"epub": "epub", "kepub": "kepub"}
	// These additionally need kindlegen.
	fb2cKindleTypes = map[string]string{"mobi": "mobi", "azw3": "azw3"}
)

type backend struct {
	bin   string
	types map[string]string // format key → --to value
}

// Converter runs the installed tools. A nil *Converter is valid and
// supports nothing.
type Converter struct {
	byFormat map[string]*backend
	sem      chan struct{}
	pending  atomic.Int32 // running + waiting conversions
}

// Find locates the converters: in dir (when given), next to the running
// executable, then in PATH. It returns nil when none is installed.
func Find(dir string) *Converter {
	dirs := []string{}
	if dir != "" {
		dirs = append(dirs, dir)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}

	c := &Converter{byFormat: map[string]*backend{}, sem: make(chan struct{}, maxParallel)}
	// fb2c first, so that fbc overrides it for the formats both can do.
	if bin := lookup("fb2c", dirs); bin != "" {
		types := map[string]string{}
		for k, v := range fb2cTypes {
			types[k] = v
		}
		// fb2c itself looks for kindlegen alongside its binary or in PATH.
		if lookup("kindlegen", []string{filepath.Dir(bin)}) != "" {
			for k, v := range fb2cKindleTypes {
				types[k] = v
			}
		}
		c.add(&backend{bin: bin, types: types})
	}
	if bin := lookup("fbc", dirs); bin != "" {
		c.add(&backend{bin: bin, types: fbcTypes})
	}
	if len(c.byFormat) == 0 {
		return nil
	}
	return c
}

func (c *Converter) add(b *backend) {
	for key := range b.types {
		c.byFormat[key] = b
	}
}

// lookup finds an executable in dirs, then in PATH; "" when absent.
func lookup(name string, dirs []string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		// Skip what cannot be run (an archive unpacked without the exec
		// bit, a stray file): a working copy may still be in PATH.
		if st, err := os.Stat(p); err == nil && runnable(st) {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func runnable(st os.FileInfo) bool {
	if !st.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || st.Mode().Perm()&0o111 != 0
}

// Supports reports whether the format can be produced.
func (c *Converter) Supports(key string) bool {
	return c != nil && c.byFormat[key] != nil
}

// Formats lists what can be produced, in display order.
func (c *Converter) Formats() []Format {
	if c == nil {
		return nil
	}
	out := make([]Format, 0, len(c.byFormat))
	for _, f := range formats {
		if c.byFormat[f.Key] != nil {
			out = append(out, f)
		}
	}
	return out
}

// Keys is Formats reduced to the format names.
func (c *Converter) Keys() []string {
	fs := c.Formats()
	keys := make([]string, 0, len(fs))
	for _, f := range fs {
		keys = append(keys, f.Key)
	}
	return keys
}

// Convert converts an FB2 document to the given format. The source is
// opened only once a conversion slot is free and is streamed to disk, so
// requests waiting their turn hold no book data.
func (c *Converter) Convert(ctx context.Context, key string, open func() (io.ReadCloser, error)) ([]byte, Format, error) {
	if !c.Supports(key) {
		return nil, Format{}, ErrUnsupported
	}
	var format Format
	for _, f := range formats {
		if f.Key == key {
			format = f
		}
	}
	b := c.byFormat[key]

	if c.pending.Add(1) > maxPending {
		c.pending.Add(-1)
		return nil, Format{}, ErrBusy
	}
	defer c.pending.Add(-1)

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, Format{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Everything happens in a private directory: both tools write their
	// logs to the working directory and name the output after the book.
	work, err := os.MkdirTemp("", "polka-convert-")
	if err != nil {
		return nil, Format{}, err
	}
	defer os.RemoveAll(work)
	outDir := filepath.Join(work, "out")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		return nil, Format{}, err
	}
	src := filepath.Join(work, "book.fb2")
	if err := writeSource(src, open); err != nil {
		return nil, Format{}, err
	}

	cmd := exec.CommandContext(ctx, b.bin, "convert", "--to", b.types[key], "--nodirs", "--overwrite", src, outDir)
	cmd.Dir = work
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, Format{}, fmt.Errorf("%s: %w: %s", filepath.Base(b.bin), err, tail(out))
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, Format{}, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), "."+format.Ext) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(outDir, e.Name()))
		if err != nil {
			return nil, Format{}, err
		}
		return data, format, nil
	}
	return nil, Format{}, fmt.Errorf("%s produced no .%s file", filepath.Base(b.bin), format.Ext)
}

func writeSource(path string, open func() (io.ReadCloser, error)) error {
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// tail keeps the end of the tool's output, where the error is.
func tail(out []byte) string {
	const max = 600
	s := strings.TrimSpace(string(out))
	if len(s) > max {
		s = "…" + s[len(s)-max:]
	}
	return s
}
