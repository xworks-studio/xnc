// probe.go — host 腿活性探测（relay-plane spec §4.3 池管理器用）。
//
// 30s 一轮对 active relay 的 host 腿发 raw QUIC 探测：握手成功 = 腿可达
// （安全组/监听/进程三关全过）；连败 2 次摘除分配，1 次成功恢复（模式
// 承接 TURN 池历史文档）。TLS 跳过校验——探测的是可达性，身份已由
// 控制连接的挑战-应答established。
package rtv

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

// ProbeHostLeg 对 host 腿地址做一次 QUIC 握手探测（连上即断）。
func ProbeHostLeg(addr string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := quic.DialAddr(ctx, addr, &tls.Config{
		InsecureSkipVerify: true, // 仅探测可达性，见文件头注释
		NextProtos:         []string{HostALPN},
	}, &quic.Config{EnableDatagrams: false})
	if err != nil {
		return false
	}
	_ = conn.CloseWithError(0, "probe")
	return true
}

// HostLegEndpoint 从描述符里取 host 腿地址（transport=quic 的第一个；
// 无则空串）。DNS 解析留给 DialAddr。
func HostLegEndpoint(eps []EndpointDescView) string {
	for _, e := range eps {
		if e.Transport == "quic" && e.Host != "" {
			h := e.Host
			if e.Port > 0 {
				h = net.JoinHostPort(h, itoa(e.Port))
			}
			return h
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// EndpointDescView 描述符的只读视图（proto.EndpointDesc 的本地形态，
// 避免 rtv 模块反向依赖 proto）。
type EndpointDescView struct {
	Transport string
	Host      string
	Port      int
}
