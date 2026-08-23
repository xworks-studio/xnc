module xnc/shellhost

go 1.26.3

require (
	github.com/Microsoft/go-winio v0.6.2
	golang.org/x/sys v0.47.0
	xnc/proto v0.0.0
)

replace xnc/proto => ../proto
