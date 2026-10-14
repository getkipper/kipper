package k8s

import (
	"context"
	"strings"

	"k8s.io/client-go/rest"
)

// WithoutPodSecurityWarnings copies cfg and filters warnings beginning with
// "would violate PodSecurity". Other warnings go to cfg's handler, or to
// rest.WarningLogger if no handler is configured.
func WithoutPodSecurityWarnings(cfg *rest.Config) *rest.Config {
	quiet := rest.CopyConfig(cfg)
	next := cfg.WarningHandlerWithContext
	if next == nil && cfg.WarningHandler != nil {
		next = legacyWarningHandler{cfg.WarningHandler}
	}
	if next == nil {
		next = rest.WarningLogger{}
	}
	quiet.WarningHandler = nil
	quiet.WarningHandlerWithContext = podSecurityFilter{next: next}
	return quiet
}

type podSecurityFilter struct {
	next rest.WarningHandlerWithContext
}

func (f podSecurityFilter) HandleWarningHeaderWithContext(ctx context.Context, code int, agent, message string) {
	if strings.HasPrefix(message, "would violate PodSecurity") {
		return
	}
	f.next.HandleWarningHeaderWithContext(ctx, code, agent, message)
}

type legacyWarningHandler struct{ rest.WarningHandler }

func (h legacyWarningHandler) HandleWarningHeaderWithContext(_ context.Context, code int, agent, message string) {
	h.HandleWarningHeader(code, agent, message)
}
