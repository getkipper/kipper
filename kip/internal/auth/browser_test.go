package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBrowserCommand(t *testing.T) {
	url := "https://dex.example.com/auth?client_id=kip&state=abc"
	all := func(string) bool { return true }
	none := func(string) bool { return false }

	name, args, ok := browserCommand("windows", none, url)
	assert.True(t, ok)
	assert.Equal(t, "rundll32", name)
	assert.Equal(t, []string{"url.dll,FileProtocolHandler", url}, args, "the URL travels as one argument, & and all")

	name, args, ok = browserCommand("darwin", all, url)
	assert.True(t, ok)
	assert.Equal(t, "open", name)
	assert.Equal(t, []string{url}, args)

	name, _, ok = browserCommand("linux", func(n string) bool { return n == "xdg-open" }, url)
	assert.True(t, ok)
	assert.Equal(t, "xdg-open", name)

	_, _, ok = browserCommand("linux", none, url)
	assert.False(t, ok, "without an opener the printed URL is the fallback")
}
