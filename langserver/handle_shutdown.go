package langserver

import (
	"context"

	"github.com/sourcegraph/jsonrpc2"
)

func (h *langHandler) handleShutdown(_ context.Context, conn *jsonrpc2.Conn, _ *jsonrpc2.Request) (result any, err error) {
	if h.lintTimer != nil {
		h.lintTimer.Stop()
	}

	// Close all passthrough server connections
	for key, server := range h.passthroughServers {
		if h.loglevel >= 1 {
			h.logger.Printf("shutting down passthrough server: %s", key)
		}
		
		// Try to send the server a shutdown request
		if server.conn != nil {
			_ = server.conn.Call(context.Background(), "shutdown", nil, nil)
		}
		
		// Terminate the process
		_ = server.cmd.Process.Kill()
	}

	close(h.request)
	return nil, nil
}
