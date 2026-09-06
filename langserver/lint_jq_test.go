package langserver

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestLintJQBasic(t *testing.T) {
	dir := t.TempDir()
	srcFile := filepath.Join(dir, "test.py")
	if err := os.WriteFile(srcFile, []byte("x = 1\nhi\nfoo()\n"), 0644); err != nil {
		t.Fatal(err)
	}
	uri := toURI(srcFile)

	type diagJSON struct {
		File     string `json:"file"`
		Message  string `json:"message"`
		Severity string `json:"severity"`
		Range    struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
			End struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"end"`
		} `json:"range"`
		Rule string `json:"rule"`
	}
	payload := map[string]interface{}{
		"generalDiagnostics": []diagJSON{
			{
				File:     filepath.ToSlash(srcFile),
				Message:  `"hi" is not defined`,
				Severity: "error",
				Rule:     "reportUndefinedVariable",
			},
			{
				File:     filepath.ToSlash(srcFile),
				Message:  "Expression value is unused",
				Severity: "warning",
				Rule:     "reportUnusedExpression",
			},
		},
	}
	// fix ranges
	diags := payload["generalDiagnostics"].([]diagJSON)
	diags[0].Range.Start.Line = 2
	diags[0].Range.Start.Character = 0
	diags[0].Range.End.Line = 2
	diags[0].Range.End.Character = 2
	diags[1].Range.Start.Line = 3
	diags[1].Range.Start.Character = 0
	diags[1].Range.End.Line = 3
	diags[1].Range.End.Character = 5
	payload["generalDiagnostics"] = diags

	jsonPath := filepath.Join(dir, "out.json")
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(jsonPath, b, 0644); err != nil {
		t.Fatal(err)
	}

	h := &langHandler{
		loglevel:          3,
		logger:            log.New(log.Writer(), "", log.LstdFlags),
		rootPath:          dir,
		lastPublishedURIs: make(map[string]map[DocumentURI]struct{}),
		pendingLints:      make(map[DocumentURI]eventType),
		configs: map[string][]Language{
			"python": {
				{
					LintCommand:        `cat ` + jsonPath,
					LintIgnoreExitCode: true,
					LintWorkspace:      true, // prevent auto-append of ${INPUT}
					LintJQ:             `.generalDiagnostics[] | {file, message, severity, range, rule}`,
				},
			},
		},
		files: map[DocumentURI]*File{
			uri: {
				LanguageID: "python",
				Text:       "x = 1\nhi\nfoo()\n",
			},
		},
	}

	result, err := h.lint(context.Background(), uri, eventTypeChange)
	if err != nil {
		t.Fatal(err)
	}

	var dlist []Diagnostic
	for _, v := range result {
		dlist = append(dlist, v...)
	}
	if len(dlist) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d: %+v", len(dlist), result)
	}

	foundError, foundWarning := false, false
	for _, d := range dlist {
		if d.Severity == 1 && d.Message == `"hi" is not defined` {
			foundError = true
			if d.Range.Start.Line != 2 || d.Range.Start.Character != 0 {
				t.Errorf("error range: got %+v want line=2 char=0", d.Range.Start)
			}
			if d.Code == nil || *d.Code != "reportUndefinedVariable" {
				t.Errorf("error code: got %v", d.Code)
			}
		}
		if d.Severity == 2 && d.Message == "Expression value is unused" {
			foundWarning = true
			if d.Range.Start.Line != 3 {
				t.Errorf("warning line: got %d want 3", d.Range.Start.Line)
			}
		}
	}
	if !foundError {
		t.Error("expected error diagnostic not found")
	}
	if !foundWarning {
		t.Error("expected warning diagnostic not found")
	}
}

func TestLintJQFallbackToErrorformat(t *testing.T) {
	base, _ := os.Getwd()
	file := filepath.Join(base, "foo")
	uri := toURI(file)

	h := &langHandler{
		loglevel:          3,
		logger:            log.New(log.Writer(), "", log.LstdFlags),
		rootPath:          base,
		lastPublishedURIs: make(map[string]map[DocumentURI]struct{}),
		pendingLints:      make(map[DocumentURI]eventType),
		configs: map[string][]Language{
			"vim": {
				{
					LintCommand:        `echo ` + file + `:2:8:E:No it is normal!`,
					LintIgnoreExitCode: true,
					LintStdin:          true,
					LintFormats:        []string{"%f:%l:%c:%t:%m"},
				},
			},
		},
		files: map[DocumentURI]*File{
			uri: {
				LanguageID: "vim",
				Text:       "scriptencoding utf-8\nabnormal!\n",
			},
		},
	}

	diags, err := h.lint(context.Background(), uri, eventTypeChange)
	if err != nil {
		t.Fatal(err)
	}
	dlist := diags[uri]
	if len(dlist) != 1 {
		t.Fatalf("expected 1 diagnostic via errorformat, got %d", len(dlist))
	}
	if dlist[0].Message != "No it is normal!" {
		t.Errorf("message = %q", dlist[0].Message)
	}
}

func TestLintJQInvalidJSONFallsBack(t *testing.T) {
	base, _ := os.Getwd()
	file := filepath.Join(base, "foo")
	uri := toURI(file)

	h := &langHandler{
		loglevel:          3,
		logger:            log.New(log.Writer(), "", log.LstdFlags),
		rootPath:          base,
		lastPublishedURIs: make(map[string]map[DocumentURI]struct{}),
		pendingLints:      make(map[DocumentURI]eventType),
		configs: map[string][]Language{
			"vim": {
				{
					LintCommand:        `echo not-json-at-all`,
					LintIgnoreExitCode: true,
					LintJQ:             `.items[]`,
					LintFormats:        []string{"%m"},
				},
			},
		},
		files: map[DocumentURI]*File{
			uri: {LanguageID: "vim", Text: "x\n"},
		},
	}

	_, err := h.lint(context.Background(), uri, eventTypeChange)
	if err != nil {
		t.Fatal(err)
	}
}
