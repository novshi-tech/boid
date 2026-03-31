package hostcmd

import "github.com/novshi-tech/boid/internal/sandbox"

func CommandFromArgv0(argv0 string) string {
	return sandbox.CommandFromArgv0(argv0)
}

func ShimExec(brokerSocket, command string, args []string, stdin []byte) (*ExecResponse, error) {
	return sandbox.ShimExec(brokerSocket, command, args, stdin)
}
