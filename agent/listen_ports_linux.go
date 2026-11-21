//go:build linux

package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/portconn"
)

const tcpListen = 10

type listeningSocket struct {
	addr  netip.AddrPort
	inode uint64
}

func listeningPorts(ctx context.Context, tagOf func(int) string) ([]execenv.ListeningPort, error) {
	var sockets []listeningSocket
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		found, err := sockDiag(ctx, family)
		if err != nil && ctx.Err() == nil {
			found, err = procListening(family)
		}
		if err != nil {
			return nil, err
		}
		sockets = append(sockets, found...)
	}
	tags := socketTags(ctx, sockets, tagOf)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ports := make([]execenv.ListeningPort, 0, len(sockets))
	for _, s := range sockets {
		ports = append(ports, execenv.ListeningPort{Port: int(s.addr.Port()), Address: s.addr.Addr().String(), Tag: tags[s.inode]})
	}
	return portconn.List(ports), nil
}

func sockDiag(ctx context.Context, family uint8) ([]listeningSocket, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1}); err != nil {
		return nil, err
	}
	// struct nlmsghdr followed by struct inet_diag_req_v2.
	const sequence = 1
	request := make([]byte, unix.NLMSG_HDRLEN+56)
	binary.NativeEndian.PutUint32(request, uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(request[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(request[8:], sequence)
	body := request[unix.NLMSG_HDRLEN:]
	body[0], body[1] = family, unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(body[4:], 1<<tcpListen)
	if err := unix.Sendto(fd, request, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}

	buf := make([]byte, 64<<10)
	var sockets []listeningSocket
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, _, flags, from, err := unix.Recvmsg(fd, buf, nil, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sender, ok := from.(*unix.SockaddrNetlink)
		if !ok || sender.Pid != 0 || flags&unix.MSG_TRUNC != 0 {
			return nil, errors.New("invalid socket diagnostic datagram")
		}
		messages, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, msg := range messages {
			if msg.Header.Seq != sequence || msg.Header.Flags&unix.NLM_F_DUMP_INTR != 0 {
				return nil, errors.New("socket diagnostic dump interrupted")
			}
			switch msg.Header.Type {
			case unix.NLMSG_DONE:
				if len(msg.Data) >= 4 && binary.NativeEndian.Uint32(msg.Data) != 0 {
					return nil, errors.New("socket diagnostic dump failed")
				}
				return sockets, nil
			case unix.SOCK_DIAG_BY_FAMILY:
				if socket, ok := diagSocket(msg.Data, family); ok {
					sockets = append(sockets, socket)
				}
			default:
				return nil, errors.New("unexpected socket diagnostic message")
			}
		}
	}
}

func diagSocket(body []byte, family uint8) (listeningSocket, bool) {
	// struct inet_diag_msg: the source in inet_diag_sockid is in network
	// byte order, while the inode at the end is in native byte order.
	if len(body) < 72 || body[0] != family || body[1] != tcpListen {
		return listeningSocket{}, false
	}
	address := netip.AddrFrom16([16]byte(body[8:24]))
	if family == unix.AF_INET {
		address = netip.AddrFrom4([4]byte(body[8:12]))
	}
	port := binary.BigEndian.Uint16(body[4:6])
	inode := binary.NativeEndian.Uint32(body[68:72])
	return listenerSocket(address, port, uint64(inode))
}

func procListening(family uint8) ([]listeningSocket, error) {
	path := "/proc/net/tcp"
	if family == unix.AF_INET6 {
		path += "6"
	}
	f, err := os.Open(path)
	if family == unix.AF_INET6 && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseProcListeners(bufio.NewScanner(f))
}

func parseProcListeners(scanner *bufio.Scanner) ([]listeningSocket, error) {
	var sockets []listeningSocket
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		addrHex, portHex, ok := strings.Cut(fields[1], ":")
		raw, addrErr := hex.DecodeString(addrHex)
		port, portErr := strconv.ParseUint(portHex, 16, 16)
		inode, inodeErr := strconv.ParseUint(fields[9], 10, 64)
		if !ok || addrErr != nil || portErr != nil || inodeErr != nil || (len(raw) != 4 && len(raw) != 16) {
			continue
		}
		// proc formats each of the address's native-endian u32 words as hex.
		for i := 0; i < len(raw); i += 4 {
			binary.NativeEndian.PutUint32(raw[i:], binary.BigEndian.Uint32(raw[i:]))
		}
		address, _ := netip.AddrFromSlice(raw)
		if socket, ok := listenerSocket(address, uint16(port), inode); ok {
			sockets = append(sockets, socket)
		}
	}
	return sockets, scanner.Err()
}

func listenerSocket(addr netip.Addr, port uint16, inode uint64) (listeningSocket, bool) {
	if port == 0 || portconn.AddressOrder(addr.String()) == 0 {
		return listeningSocket{}, false
	}
	return listeningSocket{netip.AddrPortFrom(addr, port), inode}, true
}

func socketTags(ctx context.Context, sockets []listeningSocket, tagOf func(int) string) map[uint64]string {
	tags := make(map[uint64]string)
	if len(sockets) == 0 {
		return tags
	}
	pending := make(map[string]uint64, len(sockets))
	for _, socket := range sockets {
		if socket.inode != 0 {
			pending[fmt.Sprintf("socket:[%d]", socket.inode)] = socket.inode
		}
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return tags
	}
	for _, proc := range procs {
		if len(pending) == 0 || ctx.Err() != nil {
			break
		}
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || !proc.IsDir() {
			continue
		}
		tag := tagOf(pid)
		if tag == "" {
			continue
		}
		dir := filepath.Join("/proc", proc.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil {
				continue
			}
			if inode, ok := pending[target]; ok {
				tags[inode] = tag
				delete(pending, target)
			}
		}
	}
	return tags
}
