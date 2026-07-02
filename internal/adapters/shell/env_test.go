package shell

import (
	"reflect"
	"testing"
)

// envSlice is the child-process env builder. Its contract is deterministic,
// sorted KEY=VALUE output with NO inheritance from os.Environ() — the
// runner-inner-child has already pivoted into the sandbox root, so a leaked
// host entry could shadow spec.Env at the child's first getenv(). These tests
// pin the sort order and the nil-on-empty behaviour that downstream exec.Cmd
// setup relies on.
func TestEnvSlice(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want []string
	}{
		{
			name: "nil map yields nil (inherit parent env via exec default)",
			in:   nil,
			want: nil,
		},
		{
			name: "empty map yields nil",
			in:   map[string]string{},
			want: nil,
		},
		{
			name: "single entry",
			in:   map[string]string{"FOO": "bar"},
			want: []string{"FOO=bar"},
		},
		{
			name: "multiple entries sorted by key",
			in:   map[string]string{"ZED": "3", "ALPHA": "1", "MID": "2"},
			want: []string{"ALPHA=1", "MID=2", "ZED=3"},
		},
		{
			name: "value may contain = and is preserved verbatim",
			in:   map[string]string{"KEY": "a=b=c"},
			want: []string{"KEY=a=b=c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := envSlice(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("envSlice(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
