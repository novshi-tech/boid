package sandbox

import "testing"

func TestStripANSIEscapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain text unchanged",
			input: "hello world\n",
			want:  "hello world\n",
		},
		{
			name:  "CSI color reset",
			input: "\x1b[0m",
			want:  "",
		},
		{
			name:  "CSI bold color",
			input: "\x1b[1;37mhello\x1b[0m",
			want:  "hello",
		},
		{
			name:  "CSI cursor position query (DSR)",
			input: "\x1b[6n",
			want:  "",
		},
		{
			name:  "CSI with private mode (bracketed paste enable)",
			input: "\x1b[?2004h",
			want:  "",
		},
		{
			name:  "CSI with private mode (bracketed paste disable)",
			input: "\x1b[?2004l",
			want:  "",
		},
		{
			name:  "OSC background color query (BEL terminated)",
			input: "\x1b]11;?\x07",
			want:  "",
		},
		{
			name:  "OSC background color query (ST terminated)",
			input: "\x1b]11;?\x1b\\",
			want:  "",
		},
		{
			name:  "OSC foreground color query",
			input: "\x1b]10;?\x07",
			want:  "",
		},
		{
			name:  "mixed: OSC and CSI around real content",
			input: "\x1b]11;?\x07\x1b[6n42\n",
			want:  "42\n",
		},
		{
			name:  "gh-style: bracketed paste + number + disable",
			input: "\x1b[?2004h42\r\n\x1b[?2004l",
			want:  "42\r\n",
		},
		{
			name:  "JSON with color escapes",
			input: "\x1b[1m123\x1b[0m",
			want:  "123",
		},
		{
			name:  "no escape in middle of text",
			input: "foo\x1b[32mbar\x1b[0mbaz",
			want:  "foobarbaz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSIEscapes(tt.input)
			if got != tt.want {
				t.Errorf("stripANSIEscapes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
