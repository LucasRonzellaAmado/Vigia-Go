package checker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"time"
)

var errNoReply = errors.New("sem resposta ao ping")

func ICMPPing(ctx context.Context, host string) (time.Duration, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return 0, err
	}
	if len(ips) == 0 {
		return 0, fmt.Errorf("nenhum IPv4 para %s", host)
	}

	conn, datagram, err := listenICMP()
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	id := uint16(os.Getpid() & 0xffff)
	seq := uint16(rand.IntN(1 << 16))
	payload := make([]byte, 16)
	binary.BigEndian.PutUint64(payload, rand.Uint64())
	binary.BigEndian.PutUint64(payload[8:], uint64(time.Now().UnixNano()))

	var dst net.Addr = &net.IPAddr{IP: ips[0]}
	if datagram {
		dst = &net.UDPAddr{IP: ips[0]}
	}

	start := time.Now()
	if _, err := conn.WriteTo(echoRequest(id, seq, payload), dst); err != nil {
		return 0, fmt.Errorf("enviar ICMP: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return 0, errNoReply
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return 0, errNoReply
			}
			return 0, fmt.Errorf("ler ICMP: %w", err)
		}
		if isEchoReply(buf[:n], id, seq, payload, datagram) {
			return time.Since(start), nil
		}
	}
}

func echoRequest(id, seq uint16, payload []byte) []byte {
	msg := make([]byte, 8+len(payload))
	msg[0] = 8
	binary.BigEndian.PutUint16(msg[4:], id)
	binary.BigEndian.PutUint16(msg[6:], seq)
	copy(msg[8:], payload)
	binary.BigEndian.PutUint16(msg[2:], icmpChecksum(msg))
	return msg
}

func isEchoReply(b []byte, id, seq uint16, payload []byte, datagram bool) bool {
	if len(b) < 8 || b[0] != 0 || b[1] != 0 {
		return false
	}
	if !datagram && binary.BigEndian.Uint16(b[4:]) != id {
		return false
	}
	return binary.BigEndian.Uint16(b[6:]) == seq && bytes.Equal(b[8:], payload)
}

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func listenRawICMP() (net.PacketConn, error) {
	c, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("abrir socket ICMP (libere net.ipv4.ping_group_range ou dê CAP_NET_RAW): %w", err)
	}
	return c, nil
}
