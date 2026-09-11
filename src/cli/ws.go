package main

import (
	"context"
	"strings"
	"time"
	"xnc/proto"

	"github.com/coder/websocket"
)

// wsReadLimit 会话 WS 单帧读上限：download 收 agent 推来的 64KiB binary
// chunk，screen 大 I 帧需 MiB 级余量；统一取 proto.MaxSessionFrameBytes。
const wsReadLimit = proto.MaxSessionFrameBytes

// dialSession connects to a session WS. wsPath is the websocketUrl from the
// exec 202 response：相对路径（主站旧路径）按 server 补全（https → wss）；
// 绝对 wss://（relay 数据面，Stage B）直接使用——不经 server 拼接。
func dialSession(server, wsPath string) (*websocket.Conn, error) {
	url := wsPath
	if !strings.HasPrefix(wsPath, "wss://") && !strings.HasPrefix(wsPath, "ws://") {
		url = strings.Replace(strings.Replace(server, "https://", "wss://", 1),
			"http://", "ws://", 1) + wsPath
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(wsReadLimit)
	return c, nil
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
