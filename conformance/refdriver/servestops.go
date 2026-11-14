package refdriver

import (
	"net"
	"os"
	"path/filepath"
	"time"
)

const flagListen = "-zm-ref-listen"

// serveStops is bug box-serve-stops: a box's listener that stops while its
// exec goes on. The serve exec becomes listenUntil; the peers scan's exec
// first stops it and waits until its port refuses. argv is the probe's,
// ending "net serve PORT" or "net peers PORT".
func (d *drv) serveStops(argv []string) []string {
	if len(argv) < 3 || argv[len(argv)-3] != "net" {
		return argv
	}
	port := argv[len(argv)-1]
	stop := filepath.Join(d.state, "serve-stop-"+port)
	switch argv[len(argv)-2] {
	case "serve":
		_ = os.Remove(stop) // a listener before this one was stopped, not this one
		self, err := os.Executable()
		if err != nil {
			return argv
		}
		return []string{self, flagListen, stop, port}
	case "peers":
		_ = os.WriteFile(stop, nil, 0o600)
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
			if err != nil {
				break
			}
			c.Close()
		}
	}
	return argv
}

// listenUntil accepts on port, closing each connection, until the file stop
// appears; then it closes the listener and runs until it is killed.
func listenUntil(stop, port string) int {
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return 66
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	for {
		if _, err := os.Stat(stop); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	ln.Close()
	for { // a select{} with no other goroutine is a deadlock the runtime ends
		time.Sleep(time.Hour)
	}
}
