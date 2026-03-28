package hostcmd

var BuiltinCommands = map[string]CommandDef{
	"git":  {Name: "git", Path: "/usr/bin/git", AllowedPatterns: []string{"*"}},
	"gh":   {Name: "gh", Path: "/usr/bin/gh", AllowedPatterns: []string{"*"}},
	"node": {Name: "node", Path: "/usr/bin/node", AllowedPatterns: []string{"*"}},
}
