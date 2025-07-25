package langserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

func (h *langHandler) handleTextDocumentFormatting(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (result any, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params DocumentFormattingParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	rng := Range{Position{-1, -1}, Position{-1, -1}}
	return h.rangeFormatRequest(params.TextDocument.URI, rng, params.Options)
}

func (h *langHandler) handleTextDocumentRangeFormatting(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params DocumentRangeFormattingParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	return h.rangeFormatRequest(params.TextDocument.URI, params.Range, params.Options)
}

func (h *langHandler) rangeFormatRequest(uri DocumentURI, rng Range, opt FormattingOptions) ([]TextEdit, error) {
	if h.formatTimer != nil {
		if h.loglevel >= 4 {
			h.logger.Printf("format debounced: %v", h.formatDebounce)
		}
		return []TextEdit{}, nil
	}

	h.mu.Lock()
	h.formatTimer = time.AfterFunc(h.formatDebounce, func() {
		h.mu.Lock()
		h.formatTimer = nil
		h.mu.Unlock()
	})
	h.mu.Unlock()
	return h.rangeFormatting(uri, rng, opt)
}

func (h *langHandler) rangeFormatting(uri DocumentURI, rng Range, options FormattingOptions) ([]TextEdit, error) {
	f, ok := h.files[uri]
	if !ok {
		return nil, fmt.Errorf("document not found: %v", uri)
	}

	fname, err := fromURI(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid uri: %v: %v", err, uri)
	}
	fname = filepath.ToSlash(fname)
	if runtime.GOOS == "windows" {
		fname = strings.ToLower(fname)
	}

	var configs []Language
	if cfgs, ok := h.configs[f.LanguageID]; ok {
		for _, cfg := range cfgs {
			if cfg.FormatCommand != "" {
				if dir := matchRootPath(fname, cfg.RootMarkers); dir == "" && cfg.RequireMarker {
					continue
				}
				configs = append(configs, cfg)
			}
		}
	}
	if cfgs, ok := h.configs[wildcard]; ok {
		for _, cfg := range cfgs {
			if cfg.FormatCommand != "" {
				configs = append(configs, cfg)
			}
		}
	}

	if len(configs) == 0 {
		if h.loglevel >= 1 {
			h.logger.Printf("format for LanguageID not supported: %v", f.LanguageID)
		}
		return nil, nil
	}

	originalText := f.Text
	text := originalText
	formatted := false

Configs:
	for _, config := range configs {
		if config.FormatCommand == "" {
			continue
		}

		var b []byte // This will hold the final formatted text

		if config.FormatInplace {
			h.logger.Printf("Using native in-place formatter: %s", config.FormatCommand)

			// 1. SAVE FIRST: Write the current buffer content to the original file.
			// This synchronizes the disk with any unsaved changes, preventing data loss.
			if err := os.WriteFile(fname, []byte(text), 0644); err != nil {
				h.logger.Printf("Error writing buffer to disk for in-place format: %v", err)
				continue Configs
			}

			// 2. FORMAT IN-PLACE: The formatter command will now modify the up-to-date file on disk.
			command := replaceCommandInputFilename(config.FormatCommand, fname, h.rootPath)

			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd", "/c", command)
			} else {
				cmd = exec.Command("sh", "-c", command)
			}
			cmd.Dir = h.findRootPath(fname, config)
			cmd.Env = append(os.Environ(), config.Env...)

			if output, err := cmd.CombinedOutput(); err != nil {
				h.logger.Printf("in-place formatter exited with error: %v, output: %s", err, string(output))
			}

			// 3. READ BACK: Read the newly modified content from the original file.
			b, err = os.ReadFile(fname)
			if err != nil {
				h.logger.Printf("Error reading file back from disk: %v", err)
				continue Configs
			}
		} else {
			// ORIGINAL STDIN/STDOUT LOGIC
			command := config.FormatCommand
			if !config.FormatStdin && !strings.Contains(command, "${INPUT}") {
				command = command + " ${INPUT}"
			}
			command = replaceCommandInputFilename(command, fname, h.rootPath)

			// Formatting Options
			for placeholder, value := range options {
				re, err := regexp.Compile(fmt.Sprintf(`\${([^:|^}]+):%s}`, placeholder))
				re2, err2 := regexp.Compile(fmt.Sprintf(`\${([^=|^}]+)=%s}`, placeholder))
				nre, nerr := regexp.Compile(fmt.Sprintf(`\${([^:|^}]+):!%s}`, placeholder))
				nre2, nerr2 := regexp.Compile(fmt.Sprintf(`\${([^=|^}]+)=!%s}`, placeholder))
				if err != nil || err2 != nil || nerr != nil || nerr2 != nil {
					h.logger.Println(command+":", err)
					continue Configs
				}
				switch v := value.(type) {
				default:
					command = re.ReplaceAllString(command, fmt.Sprintf("$1 %v", v))
					command = re2.ReplaceAllString(command, fmt.Sprintf("$1=%v", v))
				case bool:
					const FLAG = "$1"
					if v {
						command = re.ReplaceAllString(command, FLAG)
						command = re2.ReplaceAllString(command, FLAG)
					} else {
						command = nre.ReplaceAllString(command, FLAG)
						command = nre2.ReplaceAllString(command, FLAG)
					}
				}
			}
			if rng.Start.Line != -1 {
				charStart := convertRowColToIndex(text, rng.Start.Line, rng.Start.Character)
				charEnd := convertRowColToIndex(text, rng.End.Line, rng.End.Character)
				rangeOptions := map[string]int{
					"charStart": charStart, "charEnd": charEnd, "rowStart": rng.Start.Line, "colStart": rng.Start.Character, "rowEnd": rng.End.Line, "colEnd": rng.End.Character,
				}
				for placeholder, value := range rangeOptions {
					re, err := regexp.Compile(fmt.Sprintf(`\${([^:|^}]+):%s}`, placeholder))
					re2, err2 := regexp.Compile(fmt.Sprintf(`\${([^=|^}]+)=%s}`, placeholder))
					if err != nil || err2 != nil {
						h.logger.Println(command+":", err)
						continue Configs
					}
					command = re.ReplaceAllString(command, fmt.Sprintf("$1 %d", value))
					command = re2.ReplaceAllString(command, fmt.Sprintf("$1=%d", value))
				}
			}
			re := regexp.MustCompile(`\${[^}]*}`)
			command = re.ReplaceAllString(command, "")

			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd", "/c", command)
			} else {
				cmd = exec.Command("sh", "-c", command)
			}
			cmd.Dir = h.findRootPath(fname, config)
			cmd.Env = append(os.Environ(), config.Env...)
			if config.FormatStdin {
				cmd.Stdin = strings.NewReader(text)
			}

			var buf bytes.Buffer
			cmd.Stderr = &buf
			var err error
			b, err = cmd.Output()
			if err != nil {
				h.logger.Println(command+":", buf.String())
				continue
			}
		}

		formatted = true

		if h.loglevel >= 3 {
			h.logger.Println(config.FormatCommand+":", string(b))
		}
		text = strings.Replace(string(b), "\r", "", -1)
	}

	if formatted {
		if h.loglevel >= 3 {
			h.logger.Println("format succeeded")
		}
		return ComputeEdits(uri, originalText, text), nil
	}

	return nil, fmt.Errorf("format for LanguageID not supported: %v", f.LanguageID)
}
