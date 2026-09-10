// Package enroll 向控制面注册节点（POST /api/agent/enroll）。
package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"xnc/agent/identity"
	"xnc/agent/machineinfo"
)

// Enroll 用一次性 token 注册公钥；成功后把服务端分配的 NodeID 写回 k。
// 失败返回包含 proto 错误码的 error（enroll: <code>: <message>）。
func Enroll(ctx context.Context, serverURL, token string, k *identity.Key, info machineinfo.Info) error {
	body := map[string]string{
		"token": token, "hostname": info.Hostname, "machineId": info.MachineID,
		"osVersion": info.OSVersion, "agentVersion": info.AgentVersion,
		"publicKey": k.PublicKeyB64(),
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(serverURL, "/")+"/api/agent/enroll",
		bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		var out struct {
			NodeID string `json:"nodeId"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return err
		}
		k.NodeID = out.NodeID
		return nil
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "" {
		return fmt.Errorf("enroll: %s: %s", e.Error.Code, e.Error.Message)
	}
	return fmt.Errorf("enroll: status %d", resp.StatusCode)
}
