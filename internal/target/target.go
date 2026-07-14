// Package target resolves a <target> spec against the live set of Chrome tabs.
//
// Grammar (live-resolved, no stale cache):
//
//	<idprefix>      unique prefix of the CDP target id (canonical, case-insensitive)
//	url:<substr>    case-insensitive substring match on the tab URL
//	title:<substr>  case-insensitive substring match on the tab title
//	@N              1-based index into the current list order (ephemeral)
package target

import (
	"fmt"
	"strconv"
	"strings"
)

// Info identifies a resolvable tab.
type Info struct {
	ID    string
	Title string
	URL   string
}

// Error is a resolution failure carrying a stable error.code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Resolve maps a spec to exactly one target, or returns an *Error whose Code is
// one of: no_current_target, target_not_found, ambiguous_target.
func Resolve(spec string, targets []Info) (Info, *Error) {
	if spec == "" {
		return Info{}, &Error{
			Code:    "no_current_target",
			Message: `no target given and no current tab set — run "chrome-cdp list", then "use <target>" or pass --target`,
		}
	}

	switch {
	case strings.HasPrefix(spec, "url:"):
		return matchSubstring(spec[len("url:"):], targets, func(i Info) string { return i.URL }, "url")
	case strings.HasPrefix(spec, "title:"):
		return matchSubstring(spec[len("title:"):], targets, func(i Info) string { return i.Title }, "title")
	case strings.HasPrefix(spec, "@"):
		return matchIndex(spec[1:], targets)
	default:
		return matchIDPrefix(spec, targets)
	}
}

func matchIDPrefix(prefix string, targets []Info) (Info, *Error) {
	up := strings.ToUpper(prefix)
	var hits []Info
	for _, t := range targets {
		if strings.HasPrefix(strings.ToUpper(t.ID), up) {
			hits = append(hits, t)
		}
	}
	return one(hits, fmt.Sprintf("id prefix %q", prefix))
}

func matchSubstring(sub string, targets []Info, field func(Info) string, kind string) (Info, *Error) {
	low := strings.ToLower(sub)
	var hits []Info
	for _, t := range targets {
		if strings.Contains(strings.ToLower(field(t)), low) {
			hits = append(hits, t)
		}
	}
	return one(hits, fmt.Sprintf("%s substring %q", kind, sub))
}

func matchIndex(nstr string, targets []Info) (Info, *Error) {
	n, err := strconv.Atoi(nstr)
	if err != nil || n < 1 || n > len(targets) {
		return Info{}, &Error{
			Code:    "target_not_found",
			Message: fmt.Sprintf("no tab at index @%s (%d tabs open)", nstr, len(targets)),
		}
	}
	return targets[n-1], nil
}

func one(hits []Info, what string) (Info, *Error) {
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Info{}, &Error{Code: "target_not_found", Message: fmt.Sprintf("no tab matching %s", what)}
	default:
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
		}
		return Info{}, &Error{
			Code:    "ambiguous_target",
			Message: fmt.Sprintf("%s matches %d tabs (%s) — use more characters", what, len(hits), strings.Join(ids, ", ")),
		}
	}
}
