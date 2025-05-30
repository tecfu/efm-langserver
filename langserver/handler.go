package langserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/itchyny/gojq"
	"github.com/reviewdog/errorformat"
	"github.com/sourcegraph/jsonrpc2"

	"github.com/mattn/go-unicodeclass"
)

// Using wildcard from lsp.go instead of redefining here

type eventType int

const (
	eventTypeChange eventType = iota
	eventTypeSave
	eventTypeOpen
)

type lintRequest struct {
	URI       DocumentURI
	EventType eventType
}

// Config is
type Config struct {
	Version        int                    `yaml:"version"`
	LogFile        string                 `yaml:"log-file"`
	LogLevel       int                    `yaml:"log-level"       json:"logLevel"`
	Commands       *[]Command             `yaml:"commands"        json:"commands"`
	Languages      *map[string][]Language `yaml:"languages"       json:"languages"`
	RootMarkers    *[]string              `yaml:"root-markers"    json:"rootMarkers"`
	TriggerChars   []string               `yaml:"trigger-chars"   json:"triggerChars"`
	LintDebounce   Duration               `yaml:"lint-debounce"   json:"lintDebounce"`
	FormatDebounce Duration               `yaml:"format-debounce" json:"formatDebounce"`

	// Toggle support for "go to definition" requests.
	ProvideDefinition bool `yaml:"provide-definition"`

	Filename string      `yaml:"-"`
	Logger   *log.Logger `yaml:"-"`
}

// Config1 is
type Config1 struct {
	Version   int                 `yaml:"version"`
	Logger    *log.Logger         `yaml:"-"`
	Commands  []Command           `yaml:"commands"`
	Languages map[string]Language `yaml:"languages"`
}

// Passthrough defines configuration for passing through requests to another language server
type Passthrough struct {
	Command string   `yaml:"command" json:"command"`
	Args    []string `yaml:"args" json:"args"`
}

// Language is
type Language struct {
	Prefix             string            `yaml:"prefix" json:"prefix"`
	LintFormats        []string          `yaml:"lint-formats" json:"lintFormats"`
	LintStdin          bool              `yaml:"lint-stdin" json:"lintStdin"`
	LintOffset         int               `yaml:"lint-offset" json:"lintOffset"`
	LintOffsetColumns  int               `yaml:"lint-offset-columns" json:"lintOffsetColumns"`
	LintCommand        string            `yaml:"lint-command" json:"lintCommand"`
	LintIgnoreExitCode bool              `yaml:"lint-ignore-exit-code" json:"lintIgnoreExitCode"`
	LintCategoryMap    map[string]string `yaml:"lint-category-map" json:"lintCategoryMap"`
	LintSource         string            `yaml:"lint-source" json:"lintSource"`
	LintSeverity       int               `yaml:"lint-severity" json:"lintSeverity"`
	LintWorkspace      bool              `yaml:"lint-workspace" json:"lintWorkspace"`
	LintAfterOpen      bool              `yaml:"lint-after-open" json:"lintAfterOpen"`
	LintOnSave         bool              `yaml:"lint-on-save" json:"lintOnSave"`
	LintJQ             string            `yaml:"lint-jq" json:"lintJq"`
	FormatCommand      string            `yaml:"format-command" json:"formatCommand"`
	FormatCanRange     bool              `yaml:"format-can-range" json:"formatCanRange"`
	FormatStdin        bool              `yaml:"format-stdin" json:"formatStdin"`
	SymbolCommand      string            `yaml:"symbol-command" json:"symbolCommand"`
	SymbolStdin        bool              `yaml:"symbol-stdin" json:"symbolStdin"`
	SymbolFormats      []string          `yaml:"symbol-formats" json:"symbolFormats"`
	CompletionCommand  string            `yaml:"completion-command" json:"completionCommand"`
	CompletionStdin    bool              `yaml:"completion-stdin" json:"completionStdin"`
	HoverCommand       string            `yaml:"hover-command" json:"hoverCommand"`
	HoverStdin         bool              `yaml:"hover-stdin" json:"hoverStdin"`
	HoverType          string            `yaml:"hover-type" json:"hoverType"`
	HoverChars         string            `yaml:"hover-chars" json:"hoverChars"`
	Env                []string          `yaml:"env" json:"env"`
	RootMarkers        []string          `yaml:"root-markers" json:"rootMarkers"`
	RequireMarker      bool              `yaml:"require-marker" json:"requireMarker"`
	Commands           []Command         `yaml:"commands" json:"commands"`
	Passthrough        *Passthrough      `yaml:"passthrough" json:"passthrough"`
}

// NewHandler create JSON-RPC handler for this language server.
func NewHandler(config *Config) jsonrpc2.Handler {
	if config.Logger == nil {
		config.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}

	handler := &langHandler{
		loglevel:          config.LogLevel,
		logger:            config.Logger,
		commands:          *config.Commands,
		configs:           *config.Languages,
		provideDefinition: config.ProvideDefinition,
		files:             make(map[DocumentURI]*File),
		request:           make(chan lintRequest),
		lintDebounce:      time.Duration(config.LintDebounce),
		lintTimer:         nil,

		formatDebounce: time.Duration(config.FormatDebounce),
		formatTimer:    nil,
		conn:           nil,
		filename:       config.Filename,
		rootMarkers:    *config.RootMarkers,
		triggerChars:   config.TriggerChars,

		lastPublishedURIs: make(map[string]map[DocumentURI]struct{}),
		passthroughServers: make(map[string]*PassthroughServer),
	}
	
	// Log configuration information for debugging
	handler.logger.Printf("Initializing language handler with %d language configurations", len(handler.configs))
	for langID, langConfigs := range handler.configs {
		for _, cfg := range langConfigs {
			if cfg.Passthrough != nil {
				handler.logger.Printf("Found passthrough configuration for language %s: %s %v", 
					langID, cfg.Passthrough.Command, cfg.Passthrough.Args)
			}
		}
	}
	
	go handler.linter()
	return jsonrpc2.HandlerWithError(handler.handle)
}

// PassthroughServer represents a connection to another language server
type PassthroughServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	conn   *jsonrpc2.Conn
	mutex  sync.Mutex
	logger *log.Logger
	langID string
	command string
}

type langHandler struct {
	mu                sync.Mutex
	loglevel          int
	logger            *log.Logger
	commands          []Command
	configs           map[string][]Language
	provideDefinition bool
	files             map[DocumentURI]*File
	request           chan lintRequest
	lintDebounce      time.Duration
	lintTimer         *time.Timer
	formatDebounce    time.Duration
	formatTimer       *time.Timer
	conn              *jsonrpc2.Conn
	rootPath          string
	filename          string
	folders           []string
	rootMarkers       []string
	triggerChars      []string

	// lastPublishedURIs is mapping from LanguageID string to mapping of
	// whether diagnostics are published in a DocumentURI or not.
	lastPublishedURIs   map[string]map[DocumentURI]struct{}
	passthroughServers  map[string]*PassthroughServer
}

// File is
type File struct {
	LanguageID string
	Text       string
	Version    int
}

// WordAt is
func (f *File) WordAt(pos Position) string {
	lines := strings.Split(f.Text, "\n")
	if pos.Line < 0 || pos.Line >= len(lines) {
		return ""
	}
	chars := utf16.Encode([]rune(lines[pos.Line]))
	if pos.Character < 0 || pos.Character > len(chars) {
		return ""
	}
	prevPos := 0
	currPos := -1
	prevCls := unicodeclass.Invalid
	for i, char := range chars {
		currCls := unicodeclass.Is(rune(char))
		if currCls != prevCls {
			if i <= pos.Character {
				prevPos = i
			} else {
				if char == '_' {
					continue
				}
				currPos = i
				break
			}
		}
		prevCls = currCls
	}
	if currPos == -1 {
		currPos = len(chars)
	}
	return string(utf16.Decode(chars[prevPos:currPos]))
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 4 {
		return false
	}
	return unicode.IsLetter(rune(path[0])) && path[1] == ':'
}

func isWindowsDriveURI(uri string) bool {
	if len(uri) < 4 {
		return false
	}
	return uri[0] == '/' && unicode.IsLetter(rune(uri[1])) && uri[2] == ':'
}

func fromURI(uri DocumentURI) (string, error) {
	u, err := url.ParseRequestURI(string(uri))
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("only file URIs are supported, got %v", u.Scheme)
	}
	if isWindowsDriveURI(u.Path) {
		u.Path = u.Path[1:]
	}
	return u.Path, nil
}

func toURI(path string) DocumentURI {
	if isWindowsDrivePath(path) {
		path = "/" + path
	}
	return DocumentURI((&url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
	}).String())
}

func (h *langHandler) lintRequest(uri DocumentURI, eventType eventType) {
	if h.lintTimer != nil {
		h.lintTimer.Reset(h.lintDebounce)
		return
	}
	h.lintTimer = time.AfterFunc(h.lintDebounce, func() {
		h.lintTimer = nil
		h.request <- lintRequest{URI: uri, EventType: eventType}
	})
}

func (h *langHandler) logMessage(typ MessageType, message string) {
	h.conn.Notify(
		context.Background(),
		"window/logMessage",
		&LogMessageParams{
			Type:    typ,
			Message: message,
		})
}

func (h *langHandler) linter() {
	running := make(map[DocumentURI]context.CancelFunc)

	for {
		lintReq, ok := <-h.request
		if !ok {
			break
		}

		cancel, ok := running[lintReq.URI]
		if ok {
			cancel()
		}

		ctx, cancel := context.WithCancel(context.Background())
		running[lintReq.URI] = cancel

		go func() {
			uriToDiagnostics, err := h.lint(ctx, lintReq.URI, lintReq.EventType)
			if err != nil {
				h.logger.Println(err)
				return
			}

			for diagURI, diagnostics := range uriToDiagnostics {
				if diagURI == "file:" {
					diagURI = lintReq.URI
				}
				version := 0
				if _, ok := h.files[lintReq.URI]; ok {
					version = h.files[lintReq.URI].Version
				}
				h.conn.Notify(
					ctx,
					"textDocument/publishDiagnostics",
					&PublishDiagnosticsParams{
						URI:         diagURI,
						Diagnostics: diagnostics,
						Version:     version,
					})
			}
		}()
	}
}

func matchRootPath(fname string, markers []string) string {
	dir := filepath.Dir(filepath.Clean(fname))
	var prev string
	for dir != prev {
		files, _ := os.ReadDir(dir)
		for _, file := range files {
			name := file.Name()
			isDir := file.IsDir()
			for _, marker := range markers {
				if strings.HasSuffix(marker, "/") {
					if !isDir {
						continue
					}
					marker = strings.TrimRight(marker, "/")
					if ok, _ := filepath.Match(marker, name); ok {
						return dir
					}
				} else {
					if isDir {
						continue
					}
					if ok, _ := filepath.Match(marker, name); ok {
						return dir
					}
				}
			}
		}
		prev = dir
		dir = filepath.Dir(dir)
	}

	return ""
}

func (h *langHandler) findRootPath(fname string, lang Language) string {
	if dir := matchRootPath(fname, lang.RootMarkers); dir != "" {
		return dir
	}
	if dir := matchRootPath(fname, h.rootMarkers); dir != "" {
		return dir
	}

	for _, folder := range h.folders {
		if len(fname) > len(folder) && strings.EqualFold(fname[:len(folder)], folder) {
			return folder
		}
	}

	return h.rootPath
}

func isFilename(s string) bool {
	switch s {
	case "stdin", "-", "<text>", "<stdin>":
		return true
	default:
		return false
	}
}

func (h *langHandler) lint(ctx context.Context, uri DocumentURI, eventType eventType) (map[DocumentURI][]Diagnostic, error) {
	f, ok := h.files[uri]
	if !ok {
		return nil, fmt.Errorf("document not found: %v", uri)
	}

	fname, err := fromURI(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid uri: %v: %v", err, uri)
	}
	fname = filepath.ToSlash(fname)

	var configs []Language
	if cfgs, ok := h.configs[f.LanguageID]; ok {
		for _, cfg := range cfgs {
			// if we require markers and find that they dont exist we do not add the configuration
			if dir := matchRootPath(fname, cfg.RootMarkers); dir == "" && cfg.RequireMarker == true {
				continue
			}
			switch eventType {
			case eventTypeOpen:
				// if LintAfterOpen is not true, ignore didOpen
				if !cfg.LintAfterOpen {
					continue
				}
			case eventTypeChange:
				// if LintOnSave is true, ignore didChange
				if cfg.LintOnSave {
					continue
				}
			default:
			}
			if cfg.LintCommand != "" {
				configs = append(configs, cfg)
			}
		}
	}
	if cfgs, ok := h.configs[wildcard]; ok {
		for _, cfg := range cfgs {
			if cfg.LintCommand != "" {
				configs = append(configs, cfg)
			}
		}
	}

	if len(configs) == 0 {
		if h.loglevel >= 1 {
			h.logger.Printf("check configuration for linting `%s`, unable to load LanguageID", f.LanguageID)
		}
		return map[DocumentURI][]Diagnostic{}, nil
	}

	uriToDiagnostics := map[DocumentURI][]Diagnostic{
		uri: {},
	}
	publishedURIs := make(map[DocumentURI]struct{})
	for i, config := range configs {
		// To publish empty diagnostics when errors are fixed
		if config.LintWorkspace {
			for lastPublishedURI := range h.lastPublishedURIs[f.LanguageID] {
				if _, ok := uriToDiagnostics[lastPublishedURI]; !ok {
					uriToDiagnostics[lastPublishedURI] = []Diagnostic{}
				}
			}
		}

		if config.LintCommand == "" {
			continue
		}

		command := config.LintCommand
		if !config.LintStdin && !config.LintWorkspace && !strings.Contains(command, "${INPUT}") {
			command = command + " ${INPUT}"
		}
		rootPath := h.findRootPath(fname, config)
		command = replaceCommandInputFilename(command, fname, rootPath)

		formats := config.LintFormats
		if len(formats) == 0 {
			formats = []string{"%f:%l:%m", "%f:%l:%c:%m"}
		}

		efms, err := errorformat.NewErrorformat(formats)
		if err != nil {
			return nil, fmt.Errorf("invalid error-format: %v", config.LintFormats)
		}

		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd", "/c", command)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", command)
		}
		cmd.Dir = rootPath
		cmd.Env = append(os.Environ(), config.Env...)
		if config.LintStdin {
			cmd.Stdin = strings.NewReader(f.Text)
		}
		b, err := cmd.CombinedOutput()
		if err != nil {
			if succeeded(err) {
				return nil, nil
			}
		}
		// Most of lint tools exit with non-zero value. But some commands
		// return with zero value. We can not handle the output is real result
		// or output of usage. So efm-langserver ignore that command exiting
		// with zero-value. So if you want to handle the command which exit
		// with zero value, please specify lint-ignore-exit-code.
		if err == nil && !config.LintIgnoreExitCode {
			h.logMessage(LogError, "command `"+command+"` exit with zero. probably you forgot to specify `lint-ignore-exit-code: true`.")
			continue
		}
		if h.loglevel >= 3 {
			h.logger.Println("[Ran Lint Command]: "+command)
			h.logger.Println("[Lint Command Output]:", string(b))
		}
		if config.LintJQ != "" {
			var jsonData any
			if err := json.Unmarshal(b, &jsonData); err == nil {
				query, err := gojq.Parse(config.LintJQ)
				if err == nil {
					iter := query.Run(jsonData)
					for {
						v, ok := iter.Next()
						if !ok {
							break
						}
						diagMap, ok := v.(map[string]interface{})
						if !ok {
							continue
						}
						file, _ := diagMap["file"].(string)
						message, _ := diagMap["message"].(string)
						severityStr, _ := diagMap["severity"].(string)
						rule, _ := diagMap["rule"].(string)
						var rng Range
						if r, ok := diagMap["range"].(map[string]interface{}); ok {
							if s, ok := r["start"].(map[string]interface{}); ok {
								rng.Start.Line = int(safeFloat(s["line"]))
								rng.Start.Character = int(safeFloat(s["character"]))
							}
							if e, ok := r["end"].(map[string]interface{}); ok {
								rng.End.Line = int(safeFloat(e["line"]))
								rng.End.Character = int(safeFloat(e["character"]))
							}
						}
						severity := 1
						switch strings.ToLower(severityStr) {
						case "error":
							severity = 1
						case "warning":
							severity = 2
						case "information", "info":
							severity = 3
						case "hint":
							severity = 4
						}
						uriForDiag := uri
						if file != "" {
							uriForDiag = toURI(file)
						}
						diag := Diagnostic{
							Range:    rng,
							Severity: severity,
							Message:  message,
							Code:     &rule,
							Source:   nil,
						}
						uriToDiagnostics[uriForDiag] = append(uriToDiagnostics[uriForDiag], diag)
					}
					continue
				}
			}
		}
		var source *string
		if config.LintSource != "" {
			source = &configs[i].LintSource
			if h.loglevel >= 3 {
				if source != nil {
					h.logger.Println("[Lint Command Source]:" + *source)
				} else {
					h.logger.Println("[Lint Command Source]: nil")
				}
			}
		}

		var prefix string
		if config.Prefix != "" {
			prefix = fmt.Sprintf("[%s] ", config.Prefix)
		}

		scanner := efms.NewScanner(bytes.NewReader(b))
		for scanner.Scan() {
			entry := scanner.Entry()
			if !entry.Valid {
				continue
			}
			if config.LintStdin && isFilename(entry.Filename) {
				entry.Filename = fname
				path, err := filepath.Abs(entry.Filename)
				if err != nil {
					continue
				}
				path = filepath.ToSlash(path)
				if runtime.GOOS == "windows" && strings.ToLower(path) != strings.ToLower(fname) {
					continue
				} else if path != fname {
					continue
				}
			} else {
				entry.Filename = filepath.ToSlash(entry.Filename)
			}
			word := ""

			// entry.Col is expected to be one based, if the linter returns zero based we
			// have the ability to add an offset here.
			// We only add the offset if the linter reports entry.Col > 0 because 0 means the whole line
			if config.LintOffsetColumns > 0 && entry.Col > 0 {
				entry.Col = entry.Col + config.LintOffsetColumns
			}

			if entry.Lnum == 0 {
				entry.Lnum = 1 // entry.Lnum == 0 indicates the top line, set to 1 because it is subtracted later
			}

			if entry.Col == 0 {
				entry.Col = 1 // entry.Col == 0 indicates the whole line without column, set to 1 because it is subtracted later
			} else {
				word = f.WordAt(Position{Line: entry.Lnum - 1 - config.LintOffset, Character: entry.Col - 1})
			}

			// we allow the config to provide a mapping between LSP types E,W,I,N and whatever categories the linter has
			if len(config.LintCategoryMap) > 0 {
				entry.Type = []rune(config.LintCategoryMap[string(entry.Type)])[0]
			}

			severity := 1
			if config.LintSeverity != 0 {
				severity = config.LintSeverity
			}

			switch entry.Type {
			case 'E', 'e':
				severity = 1
			case 'W', 'w':
				severity = 2
			case 'I', 'i':
				severity = 3
			case 'N', 'n':
				severity = 4
			}

			diagURI := uri
			if entry.Filename != "" {
				if filepath.IsAbs(entry.Filename) {
					diagURI = toURI(entry.Filename)
				} else {
					diagURI = toURI(filepath.Join(rootPath, entry.Filename))
				}
			}
			if runtime.GOOS == "windows" {
				if strings.ToLower(string(diagURI)) != strings.ToLower(string(uri)) && !config.LintWorkspace {
					continue
				}
			} else {
				if diagURI != uri && !config.LintWorkspace {
					continue
				}
			}

			if config.LintWorkspace {
				publishedURIs[diagURI] = struct{}{}
			}
			uriToDiagnostics[diagURI] = append(uriToDiagnostics[diagURI], Diagnostic{
				Range: Range{
					Start: Position{Line: entry.Lnum - 1 - config.LintOffset, Character: entry.Col - 1},
					End:   Position{Line: entry.Lnum - 1 - config.LintOffset, Character: entry.Col - 1 + len([]rune(word))},
				},
				Code:     itoaPtrIfNotZero(entry.Nr),
				Message:  prefix + entry.Text,
				Severity: severity,
				Source:   source,
			})
		}
	}

	// Update state here as no possibility of cancelation
	for _, config := range configs {
		if config.LintWorkspace {
			h.lastPublishedURIs[f.LanguageID] = publishedURIs
			break
		}
	}
	return uriToDiagnostics, nil
}

func safeFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return 0
}

func itoaPtrIfNotZero(n int) *string {
	if n == 0 {
		return nil
	}
	s := strconv.Itoa(n)
	return &s
}

func (h *langHandler) closeFile(uri DocumentURI) error {
	delete(h.files, uri)
	return nil
}

func (h *langHandler) saveFile(uri DocumentURI) error {
	h.lintRequest(uri, eventTypeSave)
	return nil
}

func (h *langHandler) openFile(uri DocumentURI, languageID string, version int) error {
	h.logger.Printf("Opening file with language ID: %s", languageID)
	
	// Check if we have configuration for this language
	if cfgs, ok := h.configs[languageID]; ok {
		h.logger.Printf("Found %d configurations for language %s", len(cfgs), languageID)
		
		// Check for passthrough configurations
		for _, cfg := range cfgs {
			if cfg.Passthrough != nil {
				h.logger.Printf("Found passthrough configuration for %s: %s", 
					languageID, cfg.Passthrough.Command)
			}
		}
	} else {
		h.logger.Printf("No configurations found for language: %s", languageID)
	}
	
	f := &File{
		Text:       "",
		LanguageID: languageID,
		Version:    version,
	}
	h.files[uri] = f
	return nil
}

func (h *langHandler) updateFile(uri DocumentURI, text string, version *int, eventType eventType) error {
	f, ok := h.files[uri]
	if !ok {
		return fmt.Errorf("document not found: %v", uri)
	}
	f.Text = text
	if version != nil {
		f.Version = *version
	}

	h.lintRequest(uri, eventType)
	return nil
}

func (h *langHandler) configFor(uri DocumentURI) []Language {
	f, ok := h.files[uri]
	if !ok {
		return []Language{}
	}
	c, ok := h.configs[f.LanguageID]
	if !ok {
		return []Language{}
	}
	return c
}

func (h *langHandler) addFolder(folder string) {
	folder = filepath.Clean(folder)
	found := false
	for _, cur := range h.folders {
		if cur == folder {
			found = true
			break
		}
	}
	if !found {
		h.folders = append(h.folders, folder)
	}
}

// LoggingStream is a wrapper around an io.Reader and io.Writer that logs all data
type LoggingStream struct {
	r      io.Reader
	w      io.Writer
	logger *log.Logger
	langID string
	command string
}

// NewLoggingStream creates a new logging stream
func NewLoggingStream(r io.Reader, w io.Writer, logger *log.Logger, langID string, command string) *LoggingStream {
	return &LoggingStream{
		r:      r,
		w:      w,
		logger: logger,
		langID: langID,
		command: command,
	}
}

// Read implements io.Reader
func (l *LoggingStream) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err == nil && n > 0 {
		l.logger.Printf("language server passthrough %s %s: notif <-- %s", 
			l.langID, l.command, string(p[:n]))
	}
	return n, err
}

// Write implements io.Writer
func (l *LoggingStream) Write(p []byte) (int, error) {
	if len(p) > 0 {
		l.logger.Printf("language server passthrough %s %s: notif --> %s", 
			l.langID, l.command, string(p))
	}
	return l.w.Write(p)
}

// Close closes the underlying readers and writers if they implement io.Closer
func (l *LoggingStream) Close() error {
	var rerr, werr error
	if rc, ok := l.r.(io.Closer); ok {
		rerr = rc.Close()
	}
	if wc, ok := l.w.(io.Closer); ok {
		werr = wc.Close()
	}
	if rerr != nil {
		return rerr
	}
	return werr
}

// getPassthroughServer gets or creates a passthrough server for the given language
func (h *langHandler) getPassthroughServer(languageID string, passthrough *Passthrough) (*PassthroughServer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	key := fmt.Sprintf("%s:%s", languageID, passthrough.Command)
	if server, ok := h.passthroughServers[key]; ok {
		return server, nil
	}

	h.logger.Printf("Creating new passthrough server for %s using command: %s %v", 
		languageID, passthrough.Command, passthrough.Args)
	
	// Create a new server
	cmd := exec.Command(passthrough.Command, passthrough.Args...)
	cmd.Env = os.Environ()
	
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start passthrough server: %v", err)
	}

	// Create a dedicated logger for this passthrough server
	serverLogger := log.New(h.logger.Writer(), fmt.Sprintf("[PASSTHROUGH:%s] ", passthrough.Command), log.LstdFlags)
	serverLogger.Printf("Started passthrough language server process (PID: %d)", cmd.Process.Pid)
	
	server := &PassthroughServer{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		logger: serverLogger,
		langID: languageID,
		command: passthrough.Command,
	}

	// Create a logging stream that logs all data with the requested format
	loggingStream := NewLoggingStream(stdout, stdin, serverLogger, languageID, passthrough.Command)

	// Create a buffered stream using our logging stream
	stream := jsonrpc2.NewBufferedStream(loggingStream, jsonrpc2.VSCodeObjectCodec{})
	
	// Create connection with appropriate context
	server.conn = jsonrpc2.NewConn(context.Background(), stream, jsonrpc2.HandlerWithError(func(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
		// Log incoming requests from the passthrough server
		if req.Params != nil {
			serverLogger.Printf("language server passthrough %s %s: notif <-- %s %s", 
				languageID, passthrough.Command, req.Method, string(*req.Params))
		} else {
			serverLogger.Printf("language server passthrough %s %s: notif <-- %s", 
				languageID, passthrough.Command, req.Method)
		}
		
		// Just handle responses, not requests from the server
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound}
	}))
	
	h.passthroughServers[key] = server
	
	h.logger.Printf("Successfully created passthrough server for %s: %s", languageID, passthrough.Command)
	
	return server, nil
}

// stdrwc is a helper type to implement io.Reader and io.Writer with io.Closer
type stdrwc struct {
	r io.Reader
	w io.Writer
}

func (rw stdrwc) Read(p []byte) (int, error) {
	return rw.r.Read(p)
}

func (rw stdrwc) Write(p []byte) (int, error) {
	return rw.w.Write(p)
}

func (rw stdrwc) Close() error {
	var rerr, werr error
	if closer, ok := rw.r.(io.Closer); ok {
		rerr = closer.Close()
	}
	if closer, ok := rw.w.(io.Closer); ok {
		werr = closer.Close()
	}
	if rerr != nil {
		return rerr
	}
	return werr
}

// findPassthrough determines if a passthrough is configured for the given URI/request
func (h *langHandler) findPassthrough(uri DocumentURI, method string) (*Passthrough, string, bool) {
	f, ok := h.files[uri]
	if !ok {
		h.logger.Printf("findPassthrough: Document not found for URI: %s", uri)
		return nil, "", false
	}
	
	h.logger.Printf("findPassthrough: Looking for passthrough config for language: %s", f.LanguageID)
	
	if cfgs, ok := h.configs[f.LanguageID]; ok {
		for _, cfg := range cfgs {
			if cfg.Passthrough != nil {
				h.logger.Printf("findPassthrough: Found passthrough for %s: %s", 
					f.LanguageID, cfg.Passthrough.Command)
				return cfg.Passthrough, f.LanguageID, true
			}
		}
		h.logger.Printf("findPassthrough: No passthrough configurations found for language: %s", f.LanguageID)
	} else {
		h.logger.Printf("findPassthrough: No configurations found for language: %s", f.LanguageID)
	}
	
	return nil, "", false
}

func (h *langHandler) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result any, err error) {
	h.mu.Lock()
	if h.conn == nil {
		h.conn = conn
	}
	h.mu.Unlock()

	if h.loglevel >= 2 {
		h.logger.Printf("Received request: %s", req.Method)
		if req.Params != nil {
			h.logger.Printf("Request params: %s", string(*req.Params))
		}
	}

	if req.Params != nil {
		// Try to extract URI from various request types
		var uri DocumentURI
		
		switch req.Method {
		case "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave", "textDocument/didClose",
			"textDocument/formatting", "textDocument/rangeFormatting", "textDocument/documentSymbol",
			"textDocument/completion", "textDocument/definition", "textDocument/hover", "textDocument/codeAction":
			
			// These methods all have a TextDocument parameter with a URI
			var params struct {
				TextDocument struct {
					URI       DocumentURI `json:"uri"`
					LanguageID string      `json:"languageId,omitempty"`
				} `json:"textDocument"`
			}
			if err := json.Unmarshal(*req.Params, &params); err == nil {
				uri = params.TextDocument.URI
				if h.loglevel >= 2 && req.Method == "textDocument/didOpen" {
					h.logger.Printf("Opening document with language ID: %s", params.TextDocument.LanguageID)
				}
			}
		}
		
		if uri != "" {
			// Check if we have a passthrough configuration for this URI
			passthrough, langID, ok := h.findPassthrough(uri, req.Method)
			if ok {
				// Get or create the passthrough server
				server, err := h.getPassthroughServer(langID, passthrough)
				if err != nil {
					h.logger.Printf("Failed to create passthrough server: %v", err)
					h.logMessage(LogError, fmt.Sprintf("Failed to create passthrough server: %v", err))
				} else {
					// Forward the request to the passthrough server
					server.mutex.Lock()
					defer server.mutex.Unlock()
					
					if h.loglevel >= 2 {
						h.logger.Printf("Forwarding %s to passthrough server %s", req.Method, passthrough.Command)
					}
					
					// Log the request that's being sent
					if req.Params != nil {
						server.logger.Printf("language server passthrough %s %s: notif --> %s %s", 
							langID, passthrough.Command, req.Method, string(*req.Params))
					} else {
						server.logger.Printf("language server passthrough %s %s: notif --> %s", 
							langID, passthrough.Command, req.Method)
					}
					
					var result json.RawMessage
					err = server.conn.Call(ctx, req.Method, req.Params, &result)
					if err != nil {
						server.logger.Printf("Error in passthrough request: %v", err)
						if h.loglevel >= 1 {
							h.logger.Printf("Passthrough error: %v", err)
						}
						return nil, err
					}
					
					// Log the result
					if len(result) > 0 {
						server.logger.Printf("language server passthrough %s %s: notif <-- %s", 
							langID, passthrough.Command, string(result))
					} else {
						server.logger.Printf("language server passthrough %s %s: notif <-- empty response", 
							langID, passthrough.Command)
					}
					
					return result, nil
				}
			}
		}
	}

	// Handle the request with the original handler if not handled by passthrough
	switch req.Method {
	case "initialize":
		return h.handleInitialize(ctx, conn, req)
	case "initialized":
		return
	case "shutdown":
		return h.handleShutdown(ctx, conn, req)
	case "textDocument/didOpen":
		return h.handleTextDocumentDidOpen(ctx, conn, req)
	case "textDocument/didChange":
		return h.handleTextDocumentDidChange(ctx, conn, req)
	case "textDocument/didSave":
		return h.handleTextDocumentDidSave(ctx, conn, req)
	case "textDocument/didClose":
		return h.handleTextDocumentDidClose(ctx, conn, req)
	case "textDocument/formatting":
		return h.handleTextDocumentFormatting(ctx, conn, req)
	case "textDocument/rangeFormatting":
		return h.handleTextDocumentRangeFormatting(ctx, conn, req)
	case "textDocument/documentSymbol":
		return h.handleTextDocumentSymbol(ctx, conn, req)
	case "textDocument/completion":
		return h.handleTextDocumentCompletion(ctx, conn, req)
	case "textDocument/definition":
		return h.handleTextDocumentDefinition(ctx, conn, req)
	case "textDocument/hover":
		return h.handleTextDocumentHover(ctx, conn, req)
	case "textDocument/codeAction":
		return h.handleTextDocumentCodeAction(ctx, conn, req)
	case "workspace/executeCommand":
		return h.handleWorkspaceExecuteCommand(ctx, conn, req)
	case "workspace/didChangeConfiguration":
		return h.handleWorkspaceDidChangeConfiguration(ctx, conn, req)
	case "workspace/didChangeWorkspaceFolders":
		return h.handleDidChangeWorkspaceWorkspaceFolders(ctx, conn, req)
	case "workspace/workspaceFolders":
		return h.handleWorkspaceWorkspaceFolders(ctx, conn, req)
	}

	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: fmt.Sprintf("method not supported: %s", req.Method)}
}

func replaceCommandInputFilename(command, fname, rootPath string) string {
	ext := filepath.Ext(fname)
	ext = strings.TrimPrefix(ext, ".")

	command = strings.Replace(command, "${INPUT}", escapeBrackets(fname), -1)
	command = strings.Replace(command, "${FILEEXT}", ext, -1)
	command = strings.Replace(command, "${FILENAME}", escapeBrackets(filepath.FromSlash(fname)), -1)
	command = strings.Replace(command, "${ROOT}", escapeBrackets(rootPath), -1)

	return command
}

func escapeBrackets(path string) string {
	path = strings.Replace(path, "(", `\(`, -1)
	path = strings.Replace(path, ")", `\)`, -1)

	return path
}

func succeeded(err error) bool {
	exitErr, ok := err.(*exec.ExitError)
	// When the context is canceled, the process is killed,
	// and the exit code is -1
	return ok && exitErr.ExitCode() < 0
}
