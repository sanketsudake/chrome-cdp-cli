package chrome

// The `find` scoring model (RFC-0015): a deterministic, explainable ranking of
// accessibility nodes against a short plain-language query. Everything here is
// pure — no browser, no context — so the whole model is pinned by golden tests
// and tuning a weight is a visible diff.
//
// The table and weights live in this one file on purpose: tuning them must
// never require touching the traversal code in find.go.

import (
	"slices"
	"sort"
	"strings"
)

// findRoleWords maps a query word to the ARIA roles it implies. Role words are
// removed from the text tokens and matched against the node's role as a SOFT
// score component (--role is the hard filter). The table is fixed — not
// user-extensible — so behavior is identical across machines, which shared
// recipes depend on.
var findRoleWords = map[string][]string{
	"button":   {"button"},
	"link":     {"link"},
	"field":    {"textbox", "searchbox", "combobox"},
	"input":    {"textbox", "searchbox", "combobox"},
	"box":      {"textbox", "searchbox", "combobox"},
	"bar":      {"textbox", "searchbox", "combobox"},
	"checkbox": {"checkbox"},
	"tab":      {"tab"},
	"menu":     {"menu", "menuitem"},
	"heading":  {"heading"},
	"row":      {"row"},
	"icon":     {"img", "button"},
}

// Scoring weights. textWeight dominates because the query's words are the
// caller's strongest signal; the rest nudge ties in the direction a human
// would pick.
const (
	findTextWeight      = 0.8  // the token-overlap component's share
	findRoleBonus       = 0.15 // node's role is one the query's role words imply
	findRoleMismatch    = 0.05 // subtracted when role words are present but don't match
	findFocusableBonus  = 0.05 // focusable nodes are likelier to be the actionable target
	findIgnoredPenalty  = 0.3  // hidden/ignored nodes (reachable only under --all)
	findDisabledPenalty = 0.1  // found and reported, just ranked below enabled twins
	findSubstringCredit = 0.7  // a token found inside a longer evidence token

	// findPhraseFloor and findBrevityWeight MUST sum to 1: together they are
	// the text score's ceiling, so changing one without the other silently
	// rescales every score (and with it what findTextWeight means downstream).
	// TestFindTextScoreCeiling pins this.
	findPhraseFloor   = 0.7 // all-tokens-present guarantees at least this much text score
	findBrevityWeight = 0.3 // share of text score reserved for short, exact-ish names
)

// findQuery is a parsed `find` query: the text tokens left after role-word
// extraction, and the roles those words implied.
type findQuery struct {
	tokens []string
	roles  []string
}

// findCandidate is the pure-data view of one accessibility node, exactly what
// the scorer needs and nothing the traversal has to explain.
type findCandidate struct {
	role, name, value string
	ignored           bool
	disabled          bool
	focusable         bool
}

// parseFindQuery normalizes a query and splits it into text tokens and implied
// roles. Role words keep their query order in roles; duplicate roles collapse.
func parseFindQuery(q string) findQuery {
	var fq findQuery
	seen := map[string]bool{}
	for _, tok := range findTokens(q) {
		if roles, ok := findRoleWords[tok]; ok {
			for _, r := range roles {
				if !seen[r] {
					seen[r] = true
					fq.roles = append(fq.roles, r)
				}
			}
			continue
		}
		fq.tokens = append(fq.tokens, tok)
	}
	return fq
}

// findScore ranks one candidate against a parsed query, clamped to [0,1].
func findScore(fq findQuery, c findCandidate) float64 {
	if len(fq.tokens) == 0 && len(fq.roles) == 0 {
		return 0
	}
	evidence := findTokens(c.name + " " + c.value)

	text := findTextScore(fq.tokens, evidence)
	if len(fq.tokens) > 0 && text == 0 {
		// No text relevance at all: boosts must not resurrect it, or a query
		// like "submit" would surface every focusable control on the page.
		return 0
	}
	s := findTextWeight * text
	if len(fq.roles) > 0 {
		if slices.Contains(fq.roles, c.role) {
			s += findRoleBonus
		} else {
			s -= findRoleMismatch
		}
	}
	if c.focusable {
		s += findFocusableBonus
	}
	if c.ignored {
		s -= findIgnoredPenalty
	}
	if c.disabled {
		s -= findDisabledPenalty
	}
	return min(1, max(0, s))
}

// findTextScore is the token-overlap component: exact phrase > all tokens
// present > partial overlap, length-normalized so a short exact name beats a
// long name that merely contains the tokens.
func findTextScore(tokens, evidence []string) float64 {
	if len(tokens) == 0 {
		// A pure role-word query ("button"): every candidate passes the text
		// stage and the role component does the ranking.
		return 1
	}
	if len(evidence) == 0 {
		return 0
	}
	if strings.Join(tokens, " ") == strings.Join(evidence, " ") {
		return 1
	}
	var matched float64
	for _, t := range tokens {
		credit := 0.0
		for _, e := range evidence {
			if e == t {
				credit = 1
				break
			}
			if strings.Contains(e, t) {
				credit = findSubstringCredit
			}
		}
		matched += credit
	}
	// A zero precision falls out of the multiplication below as zero; no early
	// return needed.
	precision := matched / float64(len(tokens))
	brevity := min(1, float64(len(tokens))/float64(len(evidence)))
	return precision * (findPhraseFloor + findBrevityWeight*brevity)
}

// findTokens lowercases, strips punctuation, splits, and canonicalizes the one
// built-in equivalence: sign-in/log-in/signin spellings collapse to "login".
// The pair exists because "find the login button" against a page that says
// "Sign in" is the single most common phrasing mismatch on the web (RFC-0015
// VS-1); further synonyms are deliberately excluded until evidence demands
// them (Open Question 4).
func findTokens(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !('a' <= r && r <= 'z') && !('0' <= r && r <= '9')
	})
	var out []string
	// The loop advances explicitly, because "sign in" consumes two fields
	// while every other token consumes one.
	for i := 0; i < len(fields); {
		tok := fields[i]
		if (tok == "sign" || tok == "log") && i+1 < len(fields) && fields[i+1] == "in" {
			out = append(out, "login")
			i += 2
			continue
		}
		if tok == "signin" {
			tok = "login"
		}
		out = append(out, tok)
		i++
	}
	return out
}

// findRanked is one candidate that cleared the score threshold: its index in
// the candidate slice, and the score it earned. The score travels WITH the
// index so a caller cannot mix up rank position and candidate position.
type findRanked struct {
	Index int
	Score float64
}

// rankFindCandidates scores every candidate and returns those clearing
// minScore, best first, ties broken by document order (stable sort).
func rankFindCandidates(fq findQuery, cands []findCandidate, minScore float64) []findRanked {
	out := make([]findRanked, 0, len(cands))
	for i, c := range cands {
		// Scores are clamped to [0,1], so the >0 test is the real gate at the
		// default minScore of 0: a zero score means "no text relevance", which
		// is never a match, however permissive the threshold.
		if s := findScore(fq, c); s >= minScore && s > 0 {
			out = append(out, findRanked{Index: i, Score: s})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}
