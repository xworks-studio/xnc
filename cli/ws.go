package main

import (
	"context"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// dialSession connects to a session WS. wsPath is the relative websocketUrl
// from the exec 202 response (includes ?token=); https servers need wss.
func dialSession(server, wsPath string) (*websocket.Conn, error) {
	url := strings.Replace(strings.Replace(server, "https://", "wss://", 1),
		"http://", "ws://", 1) + wsPath
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	return c, err
}

// readWS reads one frame, returning its kind ("text" or "binary") and data.
func readWS(ctx context.Context, ws *websocket.Conn) (string, []byte, error) {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	if typ == websocket.MessageText {
		return "text", data, nil
	}
	return "binary", data, nil
}
