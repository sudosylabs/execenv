//go:build linux

package workload

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/sudosylabs/execenv/internal/start"
)

// guestDevice is the guest's one network device.
const guestDevice = "eth0"

// configureNet gives the guest's network device the start's address and
// routes everything through the host end of its link. It speaks route
// netlink itself: the image need not carry any network tool.
func configureNet(n *start.Net) error {
	link, err := dialRoute()
	if err != nil {
		return err
	}
	defer link.close()
	if err := link.up("lo"); err != nil {
		return fmt.Errorf("workload: loopback up: %w", err)
	}
	if n == nil {
		return nil
	}
	addr := net.ParseIP(n.Address).To4()
	gateway := net.ParseIP(n.Gateway).To4()
	if addr == nil || gateway == nil || n.Prefix < 1 || n.Prefix > 30 {
		return errors.New("workload: invalid start network")
	}
	dev, err := net.InterfaceByName(guestDevice)
	if err != nil {
		return fmt.Errorf("workload: no network device: %w", err)
	}
	if err := link.setUp(dev.Index); err != nil {
		return fmt.Errorf("workload: network device up: %w", err)
	}
	// Its address.
	ifa := make([]byte, unix.SizeofIfAddrmsg)
	ifa[0] = unix.AF_INET
	ifa[1] = byte(n.Prefix)
	binary.NativeEndian.PutUint32(ifa[4:], uint32(dev.Index))
	ifa = attr(ifa, unix.IFA_LOCAL, addr)
	ifa = attr(ifa, unix.IFA_ADDRESS, addr)
	if err := link.request(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, ifa); err != nil {
		return fmt.Errorf("workload: network address: %w", err)
	}
	// Everything else through the host end.
	rt := make([]byte, unix.SizeofRtMsg)
	rt[0] = unix.AF_INET
	rt[4] = unix.RT_TABLE_MAIN
	rt[5] = unix.RTPROT_BOOT
	rt[6] = unix.RT_SCOPE_UNIVERSE
	rt[7] = unix.RTN_UNICAST
	rt = attr(rt, unix.RTA_GATEWAY, gateway)
	oif := make([]byte, 4)
	binary.NativeEndian.PutUint32(oif, uint32(dev.Index))
	rt = attr(rt, unix.RTA_OIF, oif)
	if err := link.request(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, rt); err != nil {
		return fmt.Errorf("workload: default route: %w", err)
	}
	return nil
}

func (l *routeLink) up(name string) error {
	dev, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	return l.setUp(dev.Index)
}

func (l *routeLink) setUp(index int) error {
	info := make([]byte, unix.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(info[4:], uint32(index))
	binary.NativeEndian.PutUint32(info[8:], unix.IFF_UP)
	binary.NativeEndian.PutUint32(info[12:], unix.IFF_UP)
	return l.request(unix.RTM_NEWLINK, 0, info)
}

// attr appends one route attribute, padded to its alignment.
func attr(b []byte, kind uint16, value []byte) []byte {
	head := make([]byte, unix.SizeofRtAttr)
	binary.NativeEndian.PutUint16(head[0:], uint16(unix.SizeofRtAttr+len(value)))
	binary.NativeEndian.PutUint16(head[2:], kind)
	b = append(b, head...)
	b = append(b, value...)
	for len(b)%unix.RTA_ALIGNTO != 0 {
		b = append(b, 0)
	}
	return b
}

type routeLink struct {
	fd  int
	seq uint32
}

func dialRoute() (*routeLink, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("workload: route netlink: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("workload: route netlink: %w", err)
	}
	return &routeLink{fd: fd}, nil
}

func (l *routeLink) close() { _ = unix.Close(l.fd) }

// request sends one message and waits for the kernel's acknowledgement.
func (l *routeLink) request(kind uint16, flags uint16, body []byte) error {
	l.seq++
	msg := make([]byte, unix.NLMSG_HDRLEN, unix.NLMSG_HDRLEN+len(body))
	binary.NativeEndian.PutUint32(msg[0:], uint32(unix.NLMSG_HDRLEN+len(body)))
	binary.NativeEndian.PutUint16(msg[4:], kind)
	binary.NativeEndian.PutUint16(msg[6:], flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(msg[8:], l.seq)
	msg = append(msg, body...)
	if err := unix.Sendto(l.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(l.fd, buf, 0)
		if err != nil {
			return err
		}
		replies, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, r := range replies {
			if r.Header.Seq != l.seq || r.Header.Type != unix.NLMSG_ERROR {
				continue
			}
			if len(r.Data) < 4 {
				return errors.New("workload: short netlink reply")
			}
			if code := int32(binary.NativeEndian.Uint32(r.Data[0:4])); code != 0 {
				return syscall.Errno(-code)
			}
			return nil
		}
	}
}
