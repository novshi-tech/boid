package config

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTunnelWorkspacesCallback(t *testing.T) {
	var c Config
	src := "tunnel:\n  workspaces:\n    khi: {}\n    default:\n      callback: true\n    dev:\n      callback: true\n"
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.Tunnel.CallbackWorkspaces(); !slices.Equal(got, []string{"default", "dev"}) {
		t.Fatalf("callback workspaces = %v", got)
	}

	tree := Tree{}
	if _, err := Set(tree, "tunnel.workspaces.khi.callback", []string{"true"}); err != nil {
		t.Fatal(err)
	}
	if v, ok := GetPath(tree, "tunnel.workspaces.khi.callback"); !ok || v != true {
		t.Fatalf("set callback = %v %v", v, ok)
	}
}
