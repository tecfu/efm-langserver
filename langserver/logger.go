package langserver

import (
	"context"
	"fmt"
	"log"
)

// Logger is a custom logger that emits log messages as diagnostics.
type Logger struct {
	*log.Logger
	handler *langHandler
	method  string
}

// NewLogger creates a new Logger.
func NewLogger(logger *log.Logger, handler *langHandler, method string) *Logger {
	return &Logger{
		Logger:  logger,
		handler: handler,
		method:  method,
	}
}

// Println logs a message and emits it based on the configured method.
func (l *Logger) Println(v ...interface{}) {
	l.Logger.Println(v...)
	message := fmt.Sprint(v...)
	l.emitLogMessage(message)
}

// Printf logs a formatted message and emits it based on the configured method.
func (l *Logger) Printf(format string, v ...interface{}) {
	l.Logger.Printf(format, v...)
	message := fmt.Sprintf(format, v...)
	l.emitLogMessage(message)
}

// emitLogMessage emits the log message based on the configured method.
func (l *Logger) emitLogMessage(message string) {
	switch l.method {
	case "window/showMessage":
		l.handler.conn.Notify(
			context.Background(),
			"window/showMessage",
			&ShowMessageParams{
				Type:    3, // Info
				Message: message,
			},
		)
	case "window/logMessage":
		l.handler.conn.Notify(
			context.Background(),
			"window/logMessage",
			&LogMessageParams{
				Type:    3, // Info
				Message: message,
			},
		)
	case "textDocument/publishDiagnostics":
		l.handler.emitDiagnostic(message)
	}
}
