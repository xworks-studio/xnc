package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"xnc/proto"
)

// Client is a minimal XNC REST client.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func NewClient(server, token string) *Client {
	return &Client{Base: strings.TrimRight(server, "/"), Token: token,
		HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Do performs a request against the server. Server errors decode into
// *proto.APIError ({"error":{"code","message"}}); network failures are
// reported with code "NETWORK" so ExitCode maps them to 245.
func (c *Client) Do(method, path string, body, out any) *proto.APIError {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, rdr)
	if err != nil {
		return proto.Err(0, "NETWORK", err.Error())
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return proto.Err(0, "NETWORK", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error *proto.APIError `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != nil {
			e.Error.Status = resp.StatusCode
			return e.Error
		}
		return proto.Err(resp.StatusCode, proto.CodeInternal, resp.Status)
	}
	if out != nil {
		return jsonAsAPIErr(json.NewDecoder(resp.Body).Decode(out))
	}
	return nil
}

func jsonAsAPIErr(err error) *proto.APIError {
	if err == nil {
		return nil
	}
	return proto.Err(0, proto.CodeInternal, err.Error())
}
