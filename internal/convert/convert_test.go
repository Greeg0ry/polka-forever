package convert

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTool installs a shell script standing in for a converter: it writes
// "<name>:<--to value>:<source text>" to DEST/book.<ext>, the way the real
// tools derive the output name themselves.
func fakeTool(t *testing.T, dir, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script converters are not runnable on Windows")
	}
	script := `#!/bin/sh
# convert --to TYPE --nodirs --overwrite SRC DEST
to=$3; src=$6; dest=$7
case "$to" in
  epub|epub2) ext=epub ;;
  kepub) ext=kepub.epub ;;
  fail) echo "boom: bad book" >&2; exit 3 ;;
  *) ext=$to ;;
esac
# builtins only: the tests run with an empty PATH
read -r text < "$src"
printf '` + name + `:%s:%s' "$to" "$text" > "$dest/book.$ext"
`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// src is a book source with the given content.
func src(text string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(text)), nil }
}

func TestFindNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c := Find(t.TempDir())
	if c != nil {
		t.Fatalf("expected no converter, got %v", c.Keys())
	}
	// A nil converter is usable and supports nothing.
	if len(c.Formats()) != 0 || len(c.Keys()) != 0 {
		t.Fatal("nil converter must offer no formats")
	}
	if _, _, err := c.Convert(context.Background(), "epub", src("x")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestFormatsDependOnInstalledTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	join := func(c *Converter) string { return strings.Join(c.Keys(), ",") }

	onlyFbc := t.TempDir()
	fakeTool(t, onlyFbc, "fbc")
	if got := join(Find(onlyFbc)); got != "epub,kfx,kepub,pdf" {
		t.Errorf("fbc alone: %s", got)
	}

	// fb2c without kindlegen cannot make the Kindle formats.
	onlyFb2c := t.TempDir()
	fakeTool(t, onlyFb2c, "fb2c")
	if got := join(Find(onlyFb2c)); got != "epub,kepub" {
		t.Errorf("fb2c alone: %s", got)
	}
	fakeTool(t, onlyFb2c, "kindlegen")
	if got := join(Find(onlyFb2c)); got != "epub,mobi,azw3,kepub" {
		t.Errorf("fb2c + kindlegen: %s", got)
	}

	both := t.TempDir()
	for _, name := range []string{"fbc", "fb2c", "kindlegen"} {
		fakeTool(t, both, name)
	}
	if got := join(Find(both)); got != "epub,mobi,azw3,kfx,kepub,pdf" {
		t.Errorf("both: %s", got)
	}
}

func TestConvertPicksTheRightTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	for _, name := range []string{"fbc", "fb2c", "kindlegen"} {
		fakeTool(t, dir, name)
	}
	c := Find(dir)

	for key, want := range map[string]string{
		"epub":  "fbc:epub2:<fb2/>", // the maintained tool wins where both can
		"kepub": "fbc:kepub:<fb2/>",
		"kfx":   "fbc:kfx:<fb2/>",
		"mobi":  "fb2c:mobi:<fb2/>", // only the old tool makes these
		"azw3":  "fb2c:azw3:<fb2/>",
	} {
		data, f, err := c.Convert(context.Background(), key, src("<fb2/>"))
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if string(data) != want || f.Key != key {
			t.Errorf("%s: got %q (%s), want %q", key, data, f.Key, want)
		}
	}

	if _, _, err := c.Convert(context.Background(), "docx", src("x")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("unknown format: err = %v", err)
	}
}

func TestConvertReportsToolFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	fakeTool(t, dir, "fbc")
	c := Find(dir)
	c.byFormat["epub"].types = map[string]string{"epub": "fail"}

	_, _, err := c.Convert(context.Background(), "epub", src("x"))
	if err == nil || !strings.Contains(err.Error(), "boom: bad book") {
		t.Fatalf("err = %v, want the tool's stderr", err)
	}
}

func TestConvertHonoursCancellation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	fakeTool(t, dir, "fbc")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Find(dir).Convert(ctx, "epub", src("x")); err == nil {
		t.Fatal("expected an error for a cancelled request")
	}
}

// A file that cannot be executed is not a converter: a copy in PATH wins.
func TestFindSkipsNonExecutable(t *testing.T) {
	inPath := t.TempDir()
	fakeTool(t, inPath, "fbc")
	t.Setenv("PATH", inPath)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fbc"), []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Find(dir)
	if c == nil {
		t.Fatal("the converter in PATH was not found")
	}
	if got := c.byFormat["epub"].bin; filepath.Dir(got) != inPath {
		t.Errorf("picked %s, want the executable one in PATH", got)
	}

	t.Setenv("PATH", t.TempDir())
	if c := Find(dir); c != nil {
		t.Errorf("a non-executable file was accepted as a converter: %v", c.Keys())
	}
}

// Past maxPending the request is refused before the book is even opened.
func TestConvertRefusesWhenBusy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	fakeTool(t, dir, "fbc")
	c := Find(dir)
	c.pending.Store(maxPending)

	opened := false
	_, _, err := c.Convert(context.Background(), "epub", func() (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(strings.NewReader("x")), nil
	})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if opened {
		t.Error("the source was opened for a refused request")
	}
	if got := c.pending.Load(); got != maxPending {
		t.Errorf("pending = %d after a refused request, want %d", got, maxPending)
	}
}

// A request waiting for a slot must not have opened its book yet.
func TestConvertOpensSourceOnlyWithASlot(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	fakeTool(t, dir, "fbc")
	c := Find(dir)
	for i := 0; i < maxParallel; i++ {
		c.sem <- struct{}{} // every slot is taken
	}

	ctx, cancel := context.WithCancel(context.Background())
	opened := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Convert(ctx, "epub", func() (io.ReadCloser, error) {
			opened <- struct{}{}
			return io.NopCloser(strings.NewReader("x")), nil
		})
		done <- err
	}()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected the waiting request to fail on cancellation")
	}
	select {
	case <-opened:
		t.Error("the source was opened while waiting for a slot")
	default:
	}
}
