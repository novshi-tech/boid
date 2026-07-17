package profiles

import "testing"

func TestValidateSlug_Valid(t *testing.T) {
	for _, name := range []string{"home", "work", "a", "a1", "my-laptop", "my_laptop", "work2", "x-y-z_9"} {
		if err := ValidateSlug(name); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateSlug_Invalid(t *testing.T) {
	cases := []string{
		"",           // empty
		"-work",      // leading hyphen
		"_work",      // leading underscore
		"Work",       // uppercase
		"WORK",       // uppercase
		"work name",  // space
		"work/name",  // path separator
		"../etc",     // path traversal
		"..",         // path traversal
		".",          // dot
		"work.name",  // dot inside
		"work:8080",  // colon
		"wörk",       // non-ASCII
		"work\nname", // newline
	}
	for _, name := range cases {
		if err := ValidateSlug(name); err == nil {
			t.Errorf("ValidateSlug(%q) = nil, want an error", name)
		}
	}
}
