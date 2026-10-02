// Package healthcheck shares health check normalization and validation
// between the CLI and console.
package healthcheck

import (
	"fmt"
	"strings"
)

// Auto is the type that clears a declared check, so Kipper decides.
const Auto = "auto"

// Check is an App's spec.health.
type Check struct {
	Type                  string
	Path                  string
	Port                  *int32
	StartupTimeoutSeconds *int32
	TimeoutSeconds        *int32
}

// Normalize clears fields unsupported by the selected type so switching
// types preserves only applicable settings.
func (c *Check) Normalize() {
	if c.Type != "http" {
		c.Path = ""
	}
	if c.Type == "none" {
		c.Port = nil
		c.TimeoutSeconds = nil
	}
}

// Validate checks the declared health settings against the app port.
// Call Normalize first when merging an edit into an existing check.
func (c Check) Validate(appPort int32) error {
	switch c.Type {
	case "http":
		switch {
		case c.Path == "":
			return fmt.Errorf("an HTTP check needs a path, such as /healthz")
		case !strings.HasPrefix(c.Path, "/"):
			return fmt.Errorf("the check path must start with /")
		case strings.ContainsAny(c.Path, " \t\r\n"):
			return fmt.Errorf("the check path must contain no spaces or tabs or line breaks")
		case len(c.Path) > 1024:
			return fmt.Errorf("the check path is longer than 1024 characters")
		}
	case "tcp", "none":
		if c.Path != "" {
			return fmt.Errorf("only an HTTP check supports a path")
		}
		if c.Type == "none" && (c.Port != nil || c.TimeoutSeconds != nil) {
			return fmt.Errorf("a check of type none accepts no port or timeout")
		}
	default:
		return fmt.Errorf("the check type must be http, tcp or none, not %q", c.Type)
	}
	if c.Port != nil {
		if *c.Port < 1 || *c.Port > 65535 {
			return fmt.Errorf("the check port must be between 1 and 65535")
		}
		if *c.Port == appPort+10000 {
			return fmt.Errorf("port %d is the instance proxy's; check the app on its own port", *c.Port)
		}
	}
	if s := c.StartupTimeoutSeconds; s != nil && (*s < 10 || *s > 3600) {
		return fmt.Errorf("the startup timeout must be between 10 and 3600 seconds")
	}
	if s := c.TimeoutSeconds; s != nil && (*s < 1 || *s > 60) {
		return fmt.Errorf("the check timeout must be between 1 and 60 seconds")
	}
	return nil
}
