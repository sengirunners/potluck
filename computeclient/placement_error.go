package computeclient

// NoTarget means placement found no shared box binding that fits. The wrapped
// error retains the API code and hints used by existing callers.
type NoTarget struct{ Err error }

func (e *NoTarget) Error() string { return e.Err.Error() }
func (e *NoTarget) Unwrap() error { return e.Err }
