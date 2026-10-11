//go:build !linux

package checker

import "net"

func listenICMP() (net.PacketConn, bool, error) {
	c, err := listenRawICMP()
	return c, false, err
}
