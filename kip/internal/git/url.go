package git

import (
	"fmt"
	"strings"
)

// toSSH converts an HTTPS git URL to its SSH equivalent.
func toSSH(httpsURL string) string {
	// https://github.com/org/repo -> git@github.com:org/repo.git
	cleaned := strings.TrimPrefix(httpsURL, "https://")
	cleaned = strings.TrimPrefix(cleaned, "http://")
	cleaned = strings.TrimSuffix(cleaned, ".git")

	parts := strings.SplitN(cleaned, "/", 2)
	if len(parts) != 2 {
		return httpsURL
	}

	return fmt.Sprintf("git@%s:%s.git", parts[0], parts[1])
}
