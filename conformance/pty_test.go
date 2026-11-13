package conformance

// Terminals (zm-eakh.12.5.21): each way a driver can get them wrong fails
// its check (TestBrokenDrivers runs these with the rest), and a driver with
// none passes with the checks skipped.
func init() {
	brokenDrivers = append(brokenDrivers, []struct {
		kind, bug, group, want string
		edit                   func(*Options)
	}{
		{"box", "pty-unlisted", "pty", "FAIL  input answers not_implemented when not listed", nil},
		{"box", "pty-cap-only", "pty", "FAIL  a driver listing input lists capability pty", nil},
		{"box", "pty-no-tty", "pty", "FAIL  spawn with pty runs the command on a terminal of that size", nil},
		{"box", "input-ignored", "pty", "FAIL  input (base64) reaches the terminal", nil},
		{"box", "input-base64-as-text", "pty", "FAIL  input (base64) reaches the terminal", nil},
		{"box", "resize-ignored", "pty", "FAIL  input's rows and cols resize the terminal", nil},
		{"box", "input-drops-ctrl-c", "pty", "FAIL  a Ctrl-C on the terminal interrupts its command", nil},
		{"box", "input-no-pty-ok", "pty", "FAIL  input to an ended process answers running false", nil},
		{"box", "input-ended-error", "pty", "FAIL  input to an ended process answers running false", nil},
		{"box", "acquire-refused", "pty", "FAIL  acquire a box for terminals", nil},
	}...)
}
