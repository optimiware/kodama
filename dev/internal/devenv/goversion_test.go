package devenv

import "testing"

func TestCheckGoVersion(t *testing.T) {
	for _, tc := range []struct {
		found string
		ok    bool
	}{
		{"go1.27.2\n", true},
		{"go1.27.3", true},
		{"go1.28.0", true},
		{"go1.27.1", false},
		{"go1.26.9", false},
		{"devel +abc", false},
		{"", false},
	} {
		if err := checkGoVersion(tc.found, "1.27.2"); (err == nil) != tc.ok {
			t.Errorf("checkGoVersion(%q) = %v, want ok=%v", tc.found, err, tc.ok)
		}
	}
}
