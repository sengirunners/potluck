package tartpin

import "errors"

// SetBeforeMove runs f just before Install's move, for the test of a lender
// putting their own tart.app in place while an install unpacks.
func SetBeforeMove(f func()) (restore func()) {
	old := beforeMove
	beforeMove = f
	return func() { beforeMove = old }
}

// FailSwapBack lets one swap through and fails every later one.
func FailSwapBack() (restore func()) {
	n := 0
	swapApps = func(from, to string) error {
		if n++; n > 1 {
			return errors.New("swap back refused")
		}
		return swap(from, to)
	}
	return func() { swapApps = swap }
}

// OnSwap runs f with the number of each swap, before it.
func OnSwap(f func(n int)) (restore func()) {
	n := 0
	swapApps = func(from, to string) error {
		n++
		f(n)
		return swap(from, to)
	}
	return func() { swapApps = swap }
}
