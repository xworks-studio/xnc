// conn.go — 控制管道的连接协议处理（平台无关）：单行 JSON 一问一答。
// Windows（winio named pipe）与 Linux（unix socket）的 Listen/拨号在各自
// 平台文件，本文件被两侧共用。
package agentctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// handleConn 服务一条连接：读一行请求 → dispatch → 写一行应答 → 关闭。
// 读不了请求（超时/坏帧）时无法构造有意义的应答，直接关闭。
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// 各阶段独立时限：读请求 30s；dispatch（含网络往返）opTimeout；写应答
	// 30s——不能用单一 deadline（op 可能合法耗时超过它）。
	_ = conn.SetReadDeadline(time.Now().Add(connTimeout))
	line, err := readLine(conn)
	if err != nil {
		s.logger().Warn("agentctl: read request failed", "err", err)
		return
	}
	var req Request
	resp := Response{}
	if err := json.Unmarshal(line, &req); err != nil {
		resp = Errorf("bad_request: invalid json")
	} else if req.Op == "" {
		resp = Errorf("bad_request: missing op")
	} else {
		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		resp = s.dispatch(opCtx, conn, req)
		cancel()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(connTimeout))
	if err := writeJSONLine(conn, resp); err != nil {
		s.logger().Warn("agentctl: write response failed", "err", err)
	}
	s.logger().Info("agentctl op", "op", req.Op, "ok", resp.OK)
}

// readLine 读一行（'\n' 结尾；容许 EOF 无换行的最后一行），超长/超时报错。
func readLine(conn net.Conn) ([]byte, error) {
	r := bufio.NewReader(io.LimitReader(conn, maxRequestLine+1))
	line, err := r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return nil, err
	}
	if len(line) > maxRequestLine {
		return nil, fmt.Errorf("request line exceeds %d bytes", maxRequestLine)
	}
	return []byte(line), nil
}

func writeJSONLine(conn net.Conn, resp Response) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(b, '\n'))
	return err
}

// roundTripJSON 在已建立的连接上完成一次一问一答（平台 RoundTrip 共用；
// conn 由平台拨号建立，本函数负责关闭）。
func roundTripJSON(ctx context.Context, conn net.Conn, req Request) (Response, error) {
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Response{}, fmt.Errorf("agentctl: write request: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		return Response{}, fmt.Errorf("agentctl: read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("agentctl: decode response: %w", err)
	}
	return resp, nil
}
