package langserver

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFormatInplace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based in-place formatter test skipped on windows")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "sample.txt")
	original := "hello world\n"
	if err := os.WriteFile(file, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	uri := toURI(file)

	// Use sed to rewrite the file in place
	h := &langHandler{
		logger:   log.New(log.Writer(), "", log.LstdFlags),
		rootPath: dir,
		configs: map[string][]Language{
			"text": {
				{
					FormatCommand: `sed -i 's/hello/goodbye/' ${INPUT}`,
					FormatInplace: true,
				},
			},
		},
		files: map[DocumentURI]*File{
			uri: {
				LanguageID: "text",
				Text:       original,
			},
		},
	}

	rng := Range{Position{-1, -1}, Position{-1, -1}}
	edits, err := h.rangeFormatting(uri, rng, FormattingOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// After in-place format, the handler should have read the new content
	// and produced edits (or the file on disk should be updated).
	onDisk, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != "goodbye world\n" {
		t.Fatalf("on-disk content = %q, want %q", string(onDisk), "goodbye world\n")
	}

	// If edits were returned, applying them should yield the same result
	if len(edits) > 0 {
		got := applyEdits(t, original, edits)
		if got != "goodbye world\n" {
			t.Fatalf("applied edits = %q, want %q", got, "goodbye world\n")
		}
	}
}

func TestFormatStdinStillWorks(t *testing.T) {
	base, _ := os.Getwd()
	file := filepath.Join(base, "foo")
	uri := toURI(file)

	h := &langHandler{
		logger:   log.New(log.Writer(), "", log.LstdFlags),
		rootPath: base,
		configs: map[string][]Language{
			"vim": {
				{
					FormatCommand: `echo formatted`,
					FormatStdin:   true,
				},
			},
		},
		files: map[DocumentURI]*File{
			uri: {
				LanguageID: "vim",
				Text:       "original\n",
			},
		},
	}

	rng := Range{Position{-1, -1}, Position{-1, -1}}
	edits, err := h.rangeFormatting(uri, rng, FormattingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := applyEdits(t, "original\n", edits)
	// echo adds a trailing newline
	if got != "formatted\n" && got != "formatted" {
		t.Fatalf("got %q", got)
	}
}
