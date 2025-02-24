package langserver

import (
    "context"
    "log"
    "os"

    "github.com/sourcegraph/jsonrpc2"
)

func createLangServerConnection() (*jsonrpc2.Conn) {
    conn := jsonrpc2.NewConn(
        context.Background(),
        jsonrpc2.NewBufferedStream(Stdrwc{}, jsonrpc2.VSCodeObjectCodec{}),
        jsonrpc2.HandlerWithError(nil),
    )
    if conn == nil {
        log.Fatal("failed to create connection to lsp cllient")
        os.Exit(1)
    }
    return conn
}

func LogMessageStandalone(typ MessageType, message string) {
    conn := createLangServerConnection()
    noticeErr := conn.Notify(
        context.Background(),
        "window/showMessage",
        &LogMessageParams{
            Type:    typ,
            Message: message,
        },
    )

    if noticeErr != nil {
        log.Println("Failed to send notification:", noticeErr)
    }
}
