package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/agent/identity"
	"xnc/agent/machineinfo"
	"xnc/proto"
)

// fakeServer 实现最小控制面：CHALLENGE → 验签（测试内公钥）→ HELLO → HEARTBEAT echo。
func fakeServer(t *testing.T, pub ed25519.PublicKey, helloOK chan<- struct{}, beats chan<- int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		write := func(typ string, p any) {
			m, _ := proto.NewMsg(typ, p)
			b, _ := json.Marshal(m)
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		write(proto.TypeChallenge, proto.Challenge{Nonce: "test-nonce"})
		_, data, err := c.Read(ctx)
		require.NoError(t, err)
		var m proto.Message
		require.NoError(t, json.Unmarshal(data, &m))
		var cr proto.ChallengeResponse
		require.NoError(t, m.Decode(&cr))
		require.Equal(t, "node-x", cr.NodeID)
		require.True(t, ed25519.Verify(pub, []byte("test-nonce"), cr.Signature))
		write(proto.TypeHelloAck, struct{}{})
		helloOK <- struct{}{}
		n := 0
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var hm proto.Message
			require.NoError(t, json.Unmarshal(data, &hm))
			if hm.Type == proto.TypeHeartbeat {
				n++
				beats <- n
				write(proto.TypeHeartbeatAck, struct{}{})
			}
		}
	}))
}

func TestClientConnectHeartbeat(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	helloOK := make(chan struct{}, 1)
	beats := make(chan int, 8)
	srv := fakeServer(t, pub, helloOK, beats)

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	select {
	case <-helloOK:
	case <-time.After(3 * time.Second):
		t.Fatal("no HELLO_ACK")
	}
	select {
	case n := <-beats:
		assert.GreaterOrEqual(t, n, 1)
	case <-time.After(3 * time.Second):
		t.Fatal("no heartbeat")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
