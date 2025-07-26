package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sourcegraph/jsonrpc2"
	"gopkg.in/yaml.v3"

	"github.com/tecfu/efm-langserver/langserver"
)

const (
	name    = "efm-langserver"
	version = "0.0.55"
)

var revision = "HEAD"

func main() {
	var yamlfile string
	var logfile string
	var loglevel int
	var dump bool
	var showVersion bool
	var quiet bool
	var installDeps bool
	var checkDeps bool

	flag.StringVar(&yamlfile, "c", "", "path to config.yaml")
	flag.StringVar(&logfile, "logfile", "", "logfile")
	flag.IntVar(&loglevel, "loglevel", 1, "loglevel")
	flag.BoolVar(&dump, "d", false, "dump configuration")
	flag.BoolVar(&showVersion, "v", false, "Print the version")
	flag.BoolVar(&quiet, "q", false, "Run quieter")
	flag.BoolVar(&installDeps, "install-deps", false, "Install missing tool dependencies")
	flag.BoolVar(&checkDeps, "check-deps", false, "Check for missing tool dependencies")
	flag.Parse()

	if showVersion {
		fmt.Printf("%s %s (rev: %s/%s)\n", name, version, revision, runtime.Version())
		return
	}

	// Determine config file path
	if yamlfile == "" {
		var configHome string
		if runtime.GOOS == "windows" {
			configHome = os.Getenv("APPDATA")
		} else {
			configHome = os.Getenv("XDG_CONFIG_HOME")
			if configHome == "" {
				configHome = filepath.Join(os.Getenv("HOME"), ".config")
			}
		}

		dir := filepath.Join(configHome, "efm-langserver")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Fatal(err)
		}

		yamlfile = filepath.Join(dir, "config.yaml")
	} else {
		_, err := os.Stat(yamlfile)
		if err != nil {
			log.Fatal(err)
		}
	}

	config, err := langserver.LoadConfig(yamlfile)
	if err != nil {
		log.Printf("Failed to load config from %s: %v", yamlfile, err)
		log.Fatal(err)
	}

	// Set up logger for main function's output
	mainLogger := log.New(os.Stderr, "", log.LstdFlags)
	if logfile != "" {
		f, err := os.OpenFile(logfile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o660)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		mainLogger = log.New(f, "", log.LstdFlags)
	}

	if quiet {
		mainLogger.SetOutput(io.Discard)
	}

	// Handle install-deps and check-deps commands
	if installDeps || checkDeps {
		mainLogger.Println("Running dependency check/install...")
		ctx := context.Background()
		var allTools []langserver.Language
		for _, langConfigs := range *config.Languages {
			for _, toolConfig := range langConfigs {
				allTools = append(allTools, toolConfig)
			}
		}

		// Add tools defined directly under 'tools' section
		for _, toolConfig := range *config.Tools {
			allTools = append(allTools, toolConfig)
		}

		var hadError bool
		for _, toolConfig := range allTools {
			toolName := ""
			if toolConfig.LintCommand != "" {
				toolName = toolConfig.LintCommand
			} else if toolConfig.FormatCommand != "" {
				toolName = toolConfig.FormatCommand
			} else if toolConfig.CheckInstalled != "" {
				toolName = toolConfig.CheckInstalled
			} else {
				continue // Skip if no relevant command to check
			}

			err := langserver.CheckAndInstallTool(ctx, mainLogger, toolConfig, toolName, installDeps)
			if err != nil {
				hadError = true
				mainLogger.Printf("Error for tool %s: %v", toolName, err)
			}
		}

		if hadError {
			os.Exit(1)
		} else {

			mainLogger.Println("All tool dependencies checked/installed successfully.")
			os.Exit(0)
		}
	}

	if dump {
		err = yaml.NewEncoder(os.Stdout).Encode(&config)
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}

	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(1)
	}

	mainLogger.Println("efm-langserver: reading on stdin, writing on stdout")

	if logfile == "" {
		logfile = config.LogFile
	}
	if config.LogLevel > 0 {
		loglevel = config.LogLevel
	}

	var connOpt []jsonrpc2.ConnOpt

	if logfile != "" {
		f, err := os.OpenFile(logfile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o660)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		config.Logger = log.New(f, "", log.LstdFlags)
		if loglevel >= 5 {
			connOpt = append(connOpt, jsonrpc2.LogMessages(config.Logger))
		}
	}

	if quiet && (logfile == "" || loglevel < 5) {
		connOpt = append(connOpt, jsonrpc2.LogMessages(log.New(io.Discard, "", 0)))
	}

	handler := langserver.NewHandler(config)
	<-jsonrpc2.NewConn(
		context.Background(),
		jsonrpc2.NewBufferedStream(stdrwc{}, jsonrpc2.VSCodeObjectCodec{}),
		handler, connOpt...).DisconnectNotify()

	mainLogger.Println("efm-langserver: connections closed")
}

type stdrwc struct{}

func (stdrwc) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (c stdrwc) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (c stdrwc) Close() error {
	if err := os.Stdin.Close(); err != nil {
		return err
	}
	return os.Stdout.Close()
}
