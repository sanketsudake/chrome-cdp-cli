package cli

// The coordinate grammar (RFC-0014): "x,y" in CSS pixels. It is a pure
// function so the whole table is checkable without a browser, and so a
// malformed coordinate is exit 2 before Chrome is contacted.

import "testing"

func TestParsePoint(t *testing.T) {
	t.Parallel()
	ok := map[string]struct{ x, y float64 }{
		"512,340":     {512, 340},
		" 512 , 340 ": {512, 340},
		"0,0":         {0, 0},
		"12.5,40.25":  {12.5, 40.25},
		"-5,-9":       {-5, -9},
		"1e2,3":       {100, 3},
	}
	for in, want := range ok {
		t.Run("ok/"+in, func(t *testing.T) {
			t.Parallel()
			p, err := parsePoint(in)
			if err != nil {
				t.Fatalf("parsePoint(%q) errored: %v", in, err)
			}
			if p.X != want.x || p.Y != want.y {
				t.Errorf("parsePoint(%q) = %v,%v want %v,%v", in, p.X, p.Y, want.x, want.y)
			}
		})
	}
	bad := []string{"", "512", "512,", ",340", "512,340,9", "a,b", "512;340", "512 340", "NaN,3", "Inf,3"}
	for _, in := range bad {
		t.Run("bad/"+in, func(t *testing.T) {
			t.Parallel()
			if p, err := parsePoint(in); err == nil {
				t.Errorf("parsePoint(%q) = %v, want an error", in, p)
			}
		})
	}
}
