package installer

import "strings"

func replaceAll(s, old, new string) string {
	return strings.ReplaceAll(s, old, new)
}
