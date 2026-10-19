package issuelink

import (
	"net/url"
	"unicode/utf8"
)

const (
	newIssueURL   = "https://github.com/getkipper/kipper/issues/new"
	issueTemplate = "bug_report.yml"

	// MaxURLLength is the upper bound in bytes for links returned by Report.URL.
	MaxURLLength = 8000
)

// Report holds fields for the repository's bug report form. Opening its URL
// sends those fields to GitHub, so callers must supply only values kip controls
// and leave error text for the user to review and paste into the form.
type Report struct {
	Title        string
	WhatHappened string
	Repro        string
	KipVersion   string
	Environment  string
}

// URL returns a link to a prefilled issue form, omitting empty fields.
// It truncates fields at rune boundaries to query-encoded byte limits of 400
// for Title, 1000 each for WhatHappened and Repro, 100 for KipVersion and 600
// for Environment, keeping the link within MaxURLLength. It makes no request
// to GitHub and leaves submission to the user.
func (r Report) URL() string {
	q := url.Values{"template": {issueTemplate}}
	for key, field := range map[string]struct {
		value  string
		budget int
	}{
		"title":         {r.Title, 400},
		"what-happened": {r.WhatHappened, 1000},
		"repro":         {r.Repro, 1000},
		"kip-version":   {r.KipVersion, 100},
		"environment":   {r.Environment, 600},
	} {
		if v := fitEncoded(field.value, field.budget); v != "" {
			q.Set(key, v)
		}
	}
	return newIssueURL + "?" + q.Encode()
}

func fitEncoded(s string, budget int) string {
	// Each rune encodes to at least one byte, so this cut preserves any prefix that fits.
	if runes := []rune(s); len(runes) > budget {
		s = string(runes[:budget])
	}
	for len(url.QueryEscape(s)) > budget {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}
