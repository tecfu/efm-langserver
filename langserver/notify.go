package langserver

import (
    "context"
    "github.com/sourcegraph/jsonrpc2"
)

// This is allows us to show errors in the client that occur if the client could not connect (i.e. due to bad config)
func LogMessageStandalone(conn *jsonrpc2.Conn, typ MessageType, message string) {
    if conn != nil {
        conn.Notify(
            context.Background(),
            "window/showMessage",
            &LogMessageParams{
                Type:    typ,
                Message: message,
            },
        )
    }
}
