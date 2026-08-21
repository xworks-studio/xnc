// screendiag — screen 流诊断工具：发起 screen 会话，抓取二进制帧，
// 解析 SPS（真实编码分辨率/profile/level），导出 Annex-B 裸流供 ffmpeg 解码。
//
// 用法: screendiag -node LABS-TB16G7 -out dump.h264 -seconds 6
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	var (
		server  = flag.String("server", "https://control.xnc.app", "")
		token   = flag.String("token", "", "bearer token (default: ~/.xnc/config.json)")
		nodeRef = flag.String("node", "LABS-TB16G7", "")
		out     = flag.String("out", "dump.h264", "")
		seconds = flag.Int("seconds", 6, "")
	)
	flag.Parse()

	tok := *token
	if tok == "" {
		b, err := os.ReadFile(os.Getenv("USERPROFILE") + `\.xnc\config.json`)
		if err != nil {
			fatal("read config: %v", err)
		}
		var cfg struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(b, &cfg) != nil || cfg.Token == "" {
			fatal("no token in config")
		}
		tok = cfg.Token
	}

	hc := &http.Client{Timeout: 15 * time.Second}

	nodeID := *nodeRef
	if !isUUID(nodeID) {
		var nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := api(hc, *server, tok, "GET", "/api/nodes", nil, &nodes); err != nil {
			fatal("list nodes: %v", err)
		}
		for _, n := range nodes {
			if strings.EqualFold(n.Name, *nodeRef) || n.ID == *nodeRef {
				nodeID = n.ID
				break
			}
		}
	}
	fmt.Printf("node: %s\n", nodeID)

	var created struct {
		SessionID    string `json:"sessionId"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if err := api(hc, *server, tok, "POST", "/api/nodes/"+nodeID+"/screen",
		map[string]any{}, &created); err != nil {
		fatal("start screen: %v", err)
	}
	wsURL := created.WebsocketURL
	if !strings.Contains(wsURL, "://") {
		u, _ := url.Parse(*server)
		scheme := "ws"
		if u.Scheme == "https" {
			scheme = "wss"
		}
		wsURL = scheme + "://" + u.Host + wsURL
	}
	fmt.Printf("session: %s\n", created.SessionID)

	ws, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}},
	})
	if err != nil {
		fatal("dial ws: %v", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(32 << 20)

	f, err := os.Create(*out)
	if err != nil {
		fatal("create out: %v", err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*seconds)*time.Second)
	defer cancel()

	binCount, keyCount := 0, 0
	spsSeen := false
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			break // timeout / close
		}
		if typ == websocket.MessageText {
			fmt.Printf("TEXT  %s\n", string(data))
			continue
		}
		binCount++
		nalus := splitAnnexB(data)
		descs := make([]string, 0, len(nalus))
		isKey := false
		for _, n := range nalus {
			t := n[0] & 0x1F
			if t == 5 || t == 7 {
				isKey = true
			}
			if t == 7 && !spsSeen {
				spsSeen = true
				printSPS(n[1:])
			}
			descs = append(descs, fmt.Sprintf("t%d/%dB", t, len(n)))
		}
		if isKey {
			keyCount++
		}
		fmt.Printf("BIN#%03d %7dB key=%-5v [%s] head=% x\n",
			binCount, len(data), isKey, strings.Join(descs, " "), data[:min(8, len(data))])
		f.Write(data)
	}
	fmt.Printf("\ntotal binary=%d keyframes=%d sps=%v → %s\n", binCount, keyCount, spsSeen, *out)
}

func api(hc *http.Client, base, token, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, string(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "screendiag: "+f+"\n", a...)
	os.Exit(1)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// splitAnnexB 切出每个 NALU 的 header+payload（不含起始码）。
func splitAnnexB(d []byte) [][]byte {
	var out [][]byte
	i := 0
	for i < len(d) {
		hdr := -1
		for j := i; j+3 < len(d); j++ {
			if d[j] == 0 && d[j+1] == 0 {
				if d[j+2] == 1 {
					hdr = j + 3
				} else if j+4 < len(d) && d[j+2] == 0 && d[j+3] == 1 {
					hdr = j + 4
				}
				if hdr >= 0 {
					break
				}
			}
		}
		if hdr < 0 {
			break
		}
		end := len(d)
		for j := hdr + 1; j+3 <= len(d); j++ {
			if d[j] == 0 && d[j+1] == 0 && (d[j+2] == 1 || (j+4 <= len(d) && d[j+2] == 0 && d[j+3] == 1)) {
				end = j
				break
			}
		}
		for end > hdr && d[end-1] == 0 {
			end--
		}
		if hdr < end {
			out = append(out, d[hdr:end])
		}
		i = end
	}
	return out
}

// ---- SPS 解析（H.264 spec 7.3.2.1，诊断用：未处理 emulation prevention，
// 常见 SPS 不含 0x000003 序列；出错时打印原始 hex）----

type spsReader struct {
	d   []byte
	bit int
}

func (r *spsReader) u(n int) uint {
	var v uint
	for i := 0; i < n; i++ {
		byteIdx := r.bit / 8
		if byteIdx >= len(r.d) {
			break
		}
		bit := 7 - r.bit%8
		v = v<<1 | uint((r.d[byteIdx]>>bit)&1)
		r.bit++
	}
	return v
}

func (r *spsReader) ue() uint {
	zeros := 0
	for r.u(1) == 0 && zeros < 32 {
		zeros++
	}
	if zeros == 0 {
		return 0
	}
	return (1<<zeros - 1) + r.u(zeros)
}

func (r *spsReader) se() int {
	k := r.ue()
	if k%2 == 0 {
		return -int(k / 2)
	}
	return int(k+1) / 2
}

func printSPS(rbsp []byte) {
	defer func() {
		if re := recover(); re != nil {
			fmt.Printf("SPS parse panic: %v, hex=% x\n", re, rbsp)
		}
	}()
	r := &spsReader{d: rbsp}
	profile := r.u(8)
	_ = r.u(8) // constraint flags
	level := r.u(8)
	r.ue() // seq_parameter_set_id
	chroma := 1
	if profile == 100 || profile == 110 || profile == 122 || profile == 244 ||
		profile == 44 || profile == 83 || profile == 86 || profile == 118 || profile == 128 {
		chroma = int(r.ue())
		if chroma == 3 {
			r.u(1)
		}
		r.ue() // bit_depth_luma_minus8
		r.ue() // bit_depth_chroma_minus8
		r.u(1) // qpprime
		if r.u(1) != 0 {
			panic("scaling matrix present (unsupported in diag)")
		}
	}
	r.ue() // log2_max_frame_num_minus4
	pocType := int(r.ue())
	if pocType == 0 {
		r.ue()
	} else if pocType == 1 {
		r.u(1)
		r.se()
		r.se()
		for i := r.ue(); i > 0; i-- {
			r.se()
		}
	}
	r.ue() // max_num_ref_frames
	r.u(1) // gaps allowed
	wMbs := r.ue()
	hMaps := r.ue()
	frameMbsOnly := r.u(1)
	if frameMbsOnly == 0 {
		r.u(1)
	}
	r.u(1) // direct_8x8
	var crop [4]uint
	if r.u(1) != 0 {
		for i := range crop {
			crop[i] = r.ue()
		}
	}
	cropUnitX, cropUnitY := 1, int(2-frameMbsOnly)
	width := (int(wMbs) + 1) * 16
	height := (2 - int(frameMbsOnly)) * (int(hMaps) + 1) * 16
	width -= int(crop[0]+crop[1]) * cropUnitX
	height -= int(crop[2]+crop[3]) * cropUnitY
	fmt.Printf("SPS: profile=%d(0x%02X) level=%d(%.1f) mbs=%dx%d crop=%v → DISPLAY %dx%d (chroma_idc=%d)\n",
		profile, profile, level, float64(level)/10, wMbs+1, hMaps+1, crop, width, height, chroma)
}
