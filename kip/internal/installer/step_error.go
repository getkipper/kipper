package installer

// StepError wraps an install step's failure with its name, server OS and
// sizing profile so callers can report it. Run returns it when a step fails.
// Err must be non-nil.
type StepError struct {
	Step    string
	Host    HostOS
	Profile string
	Err     error
}

func (e *StepError) Error() string { return e.Step + ": " + e.Err.Error() }

func (e *StepError) Unwrap() error { return e.Err }
