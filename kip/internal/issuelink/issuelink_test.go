package issuelink

import (
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func query(t *testing.T, link string) url.Values {
	t.Helper()
	u, err := url.Parse(link)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "github.com", u.Host)
	assert.Equal(t, "/getkipper/kipper/issues/new", u.Path)
	return u.Query()
}

func TestURL_PrefillsTheBugReportForm(t *testing.T) {
	link := Report{
		Title:        `kip install failed at "Installing k3s"`,
		WhatHappened: "The install stopped.",
		Repro:        "1. Run kip install",
		KipVersion:   "v0.31.0",
		Environment:  "darwin/arm64 → ubuntu 24.04",
	}.URL()

	q := query(t, link)
	assert.Equal(t, "bug_report.yml", q.Get("template"))
	assert.Equal(t, `kip install failed at "Installing k3s"`, q.Get("title"))
	assert.Equal(t, "The install stopped.", q.Get("what-happened"))
	assert.Equal(t, "1. Run kip install", q.Get("repro"))
	assert.Equal(t, "v0.31.0", q.Get("kip-version"))
	assert.Equal(t, "darwin/arm64 → ubuntu 24.04", q.Get("environment"))
}

func TestURL_LeavesTheLogsFieldForThePastedError(t *testing.T) {
	q := query(t, Report{Title: "something broke"}.URL())

	assert.Equal(t, []string{"template", "title"}, sortedKeys(q))
}

func TestURL_FitsEvenWhenEveryFieldIsOversized(t *testing.T) {
	huge := strings.Repeat("é", MaxURLLength)
	link := Report{Title: huge, WhatHappened: huge, Repro: huge, KipVersion: huge, Environment: huge}.URL()

	assert.LessOrEqual(t, len(link), MaxURLLength)
	assert.True(t, strings.HasPrefix(query(t, link).Get("title"), "ééé"))
}

func sortedKeys(q url.Values) []string {
	var keys []string
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
