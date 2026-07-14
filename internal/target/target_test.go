package target

import "testing"

var fixture = []Info{
	{ID: "aa11", Title: "GitHub", URL: "https://github.com/"},
	{ID: "aa22", Title: "Inbox (3)", URL: "https://mail.google.com/"},
	{ID: "bb33", Title: "Localhost", URL: "http://localhost:3000/"},
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		wantID   string // "" => expect an error
		wantCode string // error code when wantID == ""
	}{
		{"unique id prefix", "aa11", "aa11", ""},
		{"id prefix case-insensitive", "BB", "bb33", ""},
		{"url substring", "url:mail.google", "aa22", ""},
		{"url substring case-insensitive", "url:GITHUB", "aa11", ""},
		{"title substring", "title:Localhost", "bb33", ""},
		{"index 1-based", "@2", "aa22", ""},
		{"ambiguous id prefix", "aa", "", "ambiguous_target"},
		{"not found id prefix", "zz", "", "target_not_found"},
		{"url not found", "url:example.org", "", "target_not_found"},
		{"index out of range", "@9", "", "target_not_found"},
		{"index zero", "@0", "", "target_not_found"},
		{"empty spec = no current target", "", "", "no_current_target"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.spec, fixture)
			if c.wantID != "" {
				if err != nil {
					t.Fatalf("Resolve(%q) unexpected error %v", c.spec, err)
				}
				if got.ID != c.wantID {
					t.Errorf("Resolve(%q) = %q, want %q", c.spec, got.ID, c.wantID)
				}
				return
			}
			if err == nil {
				t.Fatalf("Resolve(%q) = %q, want error %q", c.spec, got.ID, c.wantCode)
			}
			if err.Code != c.wantCode {
				t.Errorf("Resolve(%q) code = %q, want %q", c.spec, err.Code, c.wantCode)
			}
		})
	}
}
