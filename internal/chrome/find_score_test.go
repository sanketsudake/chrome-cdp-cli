package chrome

// Golden tests for the `find` scoring model (RFC-0015). The scorer is a pure
// function; these tables pin its ranking so weight tuning is a visible diff,
// not a silent behavior change.

import (
	"sort"
	"testing"
)

func TestParseFindQuery(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		query  string
		tokens []string
		roles  []string
	}{
		"role word extracted":        {"login button", []string{"login"}, []string{"button"}},
		"bar implies text inputs":    {"search bar", []string{"search"}, []string{"textbox", "searchbox", "combobox"}},
		"no role word":               {"settings", []string{"settings"}, nil},
		"pure role word":             {"button", nil, []string{"button"}},
		"punctuation and case":       {"Sign-In!", []string{"login"}, nil},
		"login canonicalized":        {"log in link", []string{"login"}, []string{"link"}},
		"signin canonicalized":       {"signin", []string{"login"}, nil},
		"icon implies img or button": {"delete icon", []string{"delete"}, []string{"img", "button"}},
		"heading role word":          {"settings heading", []string{"settings"}, []string{"heading"}},
		"empty":                      {"", nil, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fq := parseFindQuery(tc.query)
			if !sameStringSlices(fq.tokens, tc.tokens) {
				t.Errorf("tokens = %v, want %v", fq.tokens, tc.tokens)
			}
			if !sameStringSlices(fq.roles, tc.roles) {
				t.Errorf("roles = %v, want %v", fq.roles, tc.roles)
			}
		})
	}
}

func TestFindScoreOrdering(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		query      string
		candidates []findCandidate // want: ranked in exactly this order
	}{
		// VS-1: purpose query ranks the sign-in button above a link and heading
		// that literally contain "login".
		"login button ranks signin control first": {
			query: "login button",
			candidates: []findCandidate{
				{role: "button", name: "Sign in to your account", focusable: true},
				{role: "heading", name: "Login"},
				{role: "link", name: "Login help", focusable: true},
			},
		},
		// VS-3: role words steer role, both directions.
		"settings link over heading": {
			query: "settings link",
			candidates: []findCandidate{
				{role: "link", name: "Settings", focusable: true},
				{role: "heading", name: "Settings"},
			},
		},
		"settings heading over link": {
			query: "settings heading",
			candidates: []findCandidate{
				{role: "heading", name: "Settings"},
				{role: "link", name: "Settings", focusable: true},
			},
		},
		// VS-2: placeholder-derived name counts as evidence.
		"search bar finds placeholder textbox": {
			query: "search bar",
			candidates: []findCandidate{
				{role: "textbox", name: "Search projects", focusable: true},
				{role: "button", name: "Search", focusable: true},
			},
		},
		// Short exact name beats a long name that merely contains the tokens.
		"exact short name beats long containing name": {
			query: "save",
			candidates: []findCandidate{
				{role: "button", name: "Save", focusable: true},
				{role: "button", name: "Save your draft for later", focusable: true},
			},
		},
		// VS-12: disabled is found but ranked below an enabled near-match.
		"disabled penalized not excluded": {
			query: "save",
			candidates: []findCandidate{
				{role: "button", name: "Save draft", focusable: true},
				{role: "button", name: "Save", disabled: true},
			},
		},
		// VS-5 (--all): an ignored (hidden) node ranks below its visible twin.
		"ignored ranks below visible twin": {
			query: "submit",
			candidates: []findCandidate{
				{role: "button", name: "Submit", focusable: true},
				{role: "button", name: "Submit", focusable: true, ignored: true},
			},
		},
		// VS-11: verbose accessible names still match their visible fragment.
		"verbose workday name matches fragment": {
			query: "review button",
			candidates: []findCandidate{
				{role: "button", name: "Review Approval: Awaiting Action by Sanket", focusable: true},
				{role: "heading", name: "Needs review"},
			},
		},
		// The field's current value is evidence too.
		"value counts as evidence": {
			query: "acme field",
			candidates: []findCandidate{
				{role: "textbox", name: "Company", value: "Acme Corp", focusable: true},
				{role: "textbox", name: "Country", focusable: true},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fq := parseFindQuery(tc.query)
			type scored struct {
				i int
				s float64
			}
			got := make([]scored, len(tc.candidates))
			for i, c := range tc.candidates {
				got[i] = scored{i, findScore(fq, c)}
			}
			sort.SliceStable(got, func(a, b int) bool { return got[a].s > got[b].s })
			for rank, g := range got {
				if g.i != rank {
					t.Fatalf("rank %d is candidate %d (%q, score %.3f); want candidate %d — full: %v",
						rank, g.i, tc.candidates[g.i].name, g.s, rank, got)
				}
			}
			// Strict ordering: ties would make the ranking depend on input order.
			for i := 1; i < len(got); i++ {
				if got[i-1].s <= got[i].s {
					t.Errorf("no strict ordering between rank %d (%.3f) and %d (%.3f)", i-1, got[i-1].s, i, got[i].s)
				}
			}
		})
	}
}

func TestFindScoreBounds(t *testing.T) {
	t.Parallel()
	cases := map[string]findCandidate{
		"everything boosts":    {role: "button", name: "Save", focusable: true},
		"everything penalizes": {role: "note", name: "unrelated text entirely", ignored: true, disabled: true},
		"empty candidate":      {},
	}
	fq := parseFindQuery("save button")
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := findScore(fq, c)
			if s < 0 || s > 1 {
				t.Errorf("score %.3f out of [0,1]", s)
			}
		})
	}
}

// A no-token, no-role query must not panic and must score everything 0.
func TestFindScoreEmptyQuery(t *testing.T) {
	t.Parallel()
	fq := parseFindQuery("")
	if s := findScore(fq, findCandidate{role: "button", name: "Save"}); s != 0 {
		t.Errorf("empty query scored %.3f, want 0", s)
	}
}

func sameStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
