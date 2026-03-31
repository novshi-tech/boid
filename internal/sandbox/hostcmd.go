package sandbox

import "github.com/novshi-tech/boid/internal/hostcmd"

type CommandDef = hostcmd.CommandDef
type TokenContext = hostcmd.TokenContext
type ExecRequest = hostcmd.ExecRequest
type ExecResponse = hostcmd.ExecResponse
type Broker = hostcmd.Broker

type SecretResolver = hostcmd.SecretResolver

func CommandFromArgv0(argv0 string) string {
	return hostcmd.CommandFromArgv0(argv0)
}

func ShimExec(brokerSocket, command string, args []string, stdin []byte) (*ExecResponse, error) {
	return hostcmd.ShimExec(brokerSocket, command, args, stdin)
}
