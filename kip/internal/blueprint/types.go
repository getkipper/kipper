package blueprint

// Metadata describes a blueprint — its name, description, and configurable parameters.
type Metadata struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Version     string      `yaml:"version"`
	Parameters  []Parameter `yaml:"parameters,omitempty"`
}

// Parameter is a configurable value that the user provides at install time.
type Parameter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required,omitempty"`
	Default     string `yaml:"default,omitempty"`
}

// Blueprint is a parsed blueprint file with metadata and the raw template body.
type Blueprint struct {
	Metadata Metadata
	Template string // raw YAML template (second document)
}
