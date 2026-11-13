//go:build !linux

package vmhost

import (
	"fmt"
	"net"
)

func localDockerPeer(net.Conn) (dockerEngineOwner, error) {
	return dockerEngineOwner{}, fmt.Errorf("local Docker engine ownership verification requires Linux")
}
