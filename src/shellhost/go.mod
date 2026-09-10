module xnc/shellhost

go 1.26.3

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/stretchr/testify v1.12.1
	golang.org/x/sys v0.47.0
	xnc/proto v0.0.0
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

replace xnc/proto => ../proto
