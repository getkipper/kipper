package cmd

// withNewServiceTLS defaults an omitted Postgres tls field to "required",
// matching kip service add. It returns a shallow copy when adding the field
// and the original spec otherwise.
func withNewServiceTLS(kind string, spec map[string]interface{}) map[string]interface{} {
	if !omitsServiceTLS(kind, spec) {
		return spec
	}
	return withTLS(spec, "required")
}

// withLiveServiceTLS preserves an existing Postgres service's configured TLS
// mode when the manifest omits tls, so omission alone leaves the mode unchanged.
// An explicit manifest value takes precedence. It returns a shallow copy when
// copying a non-empty live mode and the original spec otherwise.
func withLiveServiceTLS(kind string, spec, live map[string]interface{}) map[string]interface{} {
	if !omitsServiceTLS(kind, spec) {
		return spec
	}
	mode, _ := live["tls"].(string)
	if mode == "" {
		return spec
	}
	return withTLS(spec, mode)
}

func omitsServiceTLS(kind string, spec map[string]interface{}) bool {
	if kind != "Service" || spec == nil || spec["type"] != "postgres" {
		return false
	}
	_, set := spec["tls"]
	return !set
}

func withTLS(spec map[string]interface{}, mode string) map[string]interface{} {
	out := make(map[string]interface{}, len(spec)+1)
	for k, v := range spec {
		out[k] = v
	}
	out["tls"] = mode
	return out
}
