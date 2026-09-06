package langserver

import (
	"context"

	"github.com/sourcegraph/jsonrpc2"
)

func (h *langHandler) handleShutdown(_ context.Context, conn *jsonrpc2.Conn, _ *jsonrpc2.Request) (result any, err error) {
	h.shutdown()
	return nil, conn.Close()
}

func (h *langHandler) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.isShutdown {
		return
	}
	h.isShutdown = true
	if h.lintTimer != nil {
		h.lintTimer.Stop()
		h.lintTimer = nil
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
		if server.cmd != nil && server.cmd.Process != nil {
			_ = server.cmd.Process.Kill()
		}
	}

	close(h.request)
}
