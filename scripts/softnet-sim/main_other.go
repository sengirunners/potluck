//go:build !darwin

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "softnet-sim: macOS only (it checks Softnet on vmnet)")
	os.Exit(2)
}
