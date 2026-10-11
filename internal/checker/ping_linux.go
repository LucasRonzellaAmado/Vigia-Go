package checker

import (
	"net"
	"os"
	"syscall"
)

func listenICMP() (net.PacketConn, bool, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMP)
	if err == nil {
		if err := syscall.Bind(fd, &syscall.SockaddrInet4{}); err == nil {
			f := os.NewFile(uintptr(fd), "icmp")
			c, err := net.FilePacketConn(f)
			f.Close()
			if err == nil {
				return c, true, nil
			}
		} else {
			syscall.Close(fd)
		}
	}
	c, err := listenRawICMP()
	return c, false, err
}
