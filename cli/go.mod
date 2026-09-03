module xnc/cli

go 1.26

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/coder/websocket v1.8.15
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.12.1
	golang.org/x/term v0.45.0
	xnc/proto v0.0.0
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace xnc/proto => ../proto
