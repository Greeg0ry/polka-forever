package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vestigiumincaligne/polka/internal/config"
)

// A stand-in for fbc: writes "converted:<--to value>" as the output book.
const fakeFbc = `#!/bin/sh
case "$3" in epub2) ext=epub ;; *) ext=$3 ;; esac
printf 'converted:%s' "$3" > "$7/book.$ext"
echo run >> "${0%/*}/runs"
`

// countConversions is how many times the stand-in converter has run.
func countConversions(t *testing.T, tools string) int {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(tools, "runs"))
	return strings.Count(string(data), "run")
}

func TestBookConvert(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script converter")
	}
	t.Setenv("PATH", t.TempDir()) // ignore converters installed on the machine
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "fbc"), []byte(fakeFbc), 0o755); err != nil {
		t.Fatal(err)
	}
	ts, bookID := newTestServerWith(t, func(c *config.Config) { c.ConverterDir = tools })
	id := itoa64(bookID)

	// The book page learns which formats to offer.
	form := getJSON(t, ts.URL+"/main/getBooks/getBookForm?selectedItemID="+id)
	var offered []string
	for _, f := range form["convertFormats"].([]any) {
		offered = append(offered, f.(string))
	}
	if got := strings.Join(offered, ","); got != "epub,kfx,kepub,pdf" {
		t.Fatalf("convertFormats = %s", got)
	}

	resp, err := http.Get(ts.URL + "/Images/convert/epub/" + id)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "converted:epub2" {
		t.Fatalf("convert -> %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/epub+zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="100.epub"` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// HEAD (download managers, link previews) must not run the converter.
	before := countConversions(t, tools)
	head, err := http.Head(ts.URL + "/Images/convert/epub/" + id)
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != http.StatusOK || countConversions(t, tools) != before {
		t.Errorf("HEAD -> %d, conversions %d -> %d", head.StatusCode, before, countConversions(t, tools))
	}

	// Formats nobody can produce, and unknown books, are plain 404s.
	for _, path := range []string{"/Images/convert/mobi/" + id, "/Images/convert/docx/" + id, "/Images/convert/epub/999999"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s -> %d, want 404", path, resp.StatusCode)
		}
	}
}

// Without a converter installed nothing is offered and the route is a 404.
func TestBookConvertUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ts, bookID := newTestServerWith(t, func(c *config.Config) { c.ConverterDir = t.TempDir() })
	id := itoa64(bookID)

	form := getJSON(t, ts.URL+"/main/getBooks/getBookForm?selectedItemID="+id)
	if _, ok := form["convertFormats"]; ok {
		t.Error("convertFormats offered without a converter")
	}
	resp, err := http.Get(ts.URL + "/Images/convert/epub/" + id)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("convert without a converter -> %d, want 404", resp.StatusCode)
	}
}
