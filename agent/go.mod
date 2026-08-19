module xnc/agent

go 1.26

require (
	github.com/coder/websocket v1.8.15
	github.com/stretchr/testify v1.12.1
	golang.org/x/sys v0.47.0
	xnc/proto v0.0.0
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

replace xnc/proto => ../proto
