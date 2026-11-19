package vmhost

// guestHome uses the login user's home when the image leaves HOME unset or
// empty. It is safe in wrappers with nounset enabled.
const guestHome = `[ -n "${HOME:-}" ] || HOME=$(cd ~ && pwd); export HOME
`
