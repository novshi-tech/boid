package apiwire

import "testing"

func TestEffectiveDisplayName(t *testing.T) {
	for _, tc := range []struct {
		job  Job
		want string
	}{
		{Job{DisplayName: "manual", TerminalTitle: "terminal"}, "manual"},
		{Job{DisplayName: "default", DisplayNameDefault: true, TerminalTitle: "terminal"}, "terminal"},
		{Job{DisplayName: "default", DisplayNameDefault: true}, "default"},
		{Job{TerminalTitle: "terminal"}, "terminal"},
		{Job{}, ""},
	} {
		if got := tc.job.EffectiveDisplayName(); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.job, got, tc.want)
		}
	}
}
