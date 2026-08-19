module xnc/mockagent

go 1.26

require xnc/agent v0.0.0

require (
	github.com/coder/websocket v1.8.15 // indirect
	golang.org/x/sys v0.47.0 // indirect
	xnc/proto v0.0.0 // indirect
)

replace xnc/agent => ../agent

replace xnc/proto => ../proto
