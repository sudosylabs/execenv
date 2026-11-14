//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

// The kernel process events connector (linux/connector.h, linux/cn_proc.h).
const (
	cnIdxProc         = 1
	cnValProc         = 1
	procMcastListen   = 1
	procEventNone     = 0x00000000
	procEventFork     = 0x00000001
	procEventExec     = 0x00000002
	procEventExit     = 0x80000000
	cnMsgBytes        = 20
	procEventHeader   = 16
	procAckTimeout    = 2 * time.Second
	procReceiveBuffer = 4 << 20
	cmdlineReadBytes  = 8 << 10
)

// openKernelProcesses subscribes to the guest kernel's process events and
// filters them to the workload hierarchy at group. It fails when the kernel
// lacks the connector or its process events, so the helper can refuse to
// advertise a process log it cannot keep.
func openKernelProcesses(group, home, scratch string) (ProcessSource, error) {
	if group == "" {
		return nil, execenv.ErrUnavailable
	}
	reader, err := openConnector()
	if err != nil {
		return nil, err
	}
	return &kernelProcesses{reader: reader, tracker: newTracker(procfs{}, group, os.Getpid(), home, scratch)}, nil
}

type connector struct {
	f       *os.File
	buf     []byte
	pending []kernelEvent
}

func openConnector() (*connector, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_CONNECTOR)
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	// A large buffer makes kernel-side loss rare; loss is still reported.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, procReceiveBuffer)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: cnIdxProc}); err != nil {
		_ = unix.Close(fd)
		return nil, execenv.ErrUnavailable
	}
	c := &connector{f: os.NewFile(uintptr(fd), "proc-connector"), buf: make([]byte, 64<<10)}
	if err := c.listen(); err != nil {
		_ = c.f.Close()
		return nil, err
	}
	return c, nil
}

// listen subscribes and waits for the kernel's acknowledgement. A kernel
// without process events never answers, and one that refuses the helper
// answers with an error.
func (c *connector) listen() error {
	message := make([]byte, unix.SizeofNlMsghdr+cnMsgBytes+4)
	order := binary.NativeEndian
	order.PutUint32(message[0:], uint32(len(message)))
	order.PutUint16(message[4:], unix.NLMSG_DONE)
	order.PutUint32(message[16:], cnIdxProc)
	order.PutUint32(message[20:], cnValProc)
	order.PutUint16(message[32:], 4)
	order.PutUint32(message[36:], procMcastListen)
	raw, err := c.f.SyscallConn()
	if err != nil {
		return execenv.ErrUnavailable
	}
	var sendErr error
	if err := raw.Write(func(fd uintptr) bool {
		sendErr = unix.Sendto(int(fd), message, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK})
		return !errors.Is(sendErr, unix.EAGAIN)
	}); err != nil || sendErr != nil {
		return execenv.ErrUnavailable
	}
	if err := c.f.SetReadDeadline(time.Now().Add(procAckTimeout)); err != nil {
		return execenv.ErrUnavailable
	}
	defer c.f.SetReadDeadline(time.Time{})
	for {
		events, ack, err := c.receive()
		if err != nil || ack == ackRefused {
			return execenv.ErrUnavailable
		}
		c.pending = append(c.pending, events...)
		if ack == ackAccepted {
			return nil
		}
	}
}

func (c *connector) next() (kernelEvent, error) {
	for len(c.pending) == 0 {
		events, _, err := c.receive()
		if err != nil {
			return kernelEvent{}, err
		}
		c.pending = events
	}
	event := c.pending[0]
	c.pending = c.pending[1:]
	return event, nil
}

func (c *connector) close() error { return c.f.Close() }

// procAck is a subscription acknowledgement in one datagram.
type procAck uint8

const (
	ackNone procAck = iota
	ackAccepted
	ackRefused
)

// receive reads one datagram. A kernel buffer overflow is one lost event.
func (c *connector) receive() ([]kernelEvent, procAck, error) {
	raw, err := c.f.SyscallConn()
	if err != nil {
		return nil, ackNone, err
	}
	var n int
	var recvErr error
	if err := raw.Read(func(fd uintptr) bool {
		n, _, recvErr = unix.Recvfrom(int(fd), c.buf, 0)
		return !errors.Is(recvErr, unix.EAGAIN)
	}); err != nil {
		return nil, ackNone, err
	}
	if errors.Is(recvErr, unix.ENOBUFS) {
		return []kernelEvent{{Kind: kernelLost, At: time.Now()}}, ackNone, nil
	}
	if recvErr != nil {
		return nil, ackNone, recvErr
	}
	events, ack := parseProcMessages(c.buf[:n], time.Now())
	return events, ack, nil
}

// parseProcMessages decodes one datagram of connector messages.
func parseProcMessages(data []byte, at time.Time) ([]kernelEvent, procAck) {
	order := binary.NativeEndian
	var events []kernelEvent
	ack := ackNone
	for len(data) >= unix.SizeofNlMsghdr {
		size := int(order.Uint32(data[0:]))
		if size < unix.SizeofNlMsghdr || size > len(data) {
			break
		}
		body := data[unix.SizeofNlMsghdr:size]
		data = data[min(len(data), (size+3)&^3):]
		if len(body) < cnMsgBytes || order.Uint32(body[0:]) != cnIdxProc || order.Uint32(body[4:]) != cnValProc {
			continue
		}
		length := int(order.Uint16(body[16:]))
		event := body[cnMsgBytes:]
		if length > len(event) || length < procEventHeader {
			continue
		}
		event = event[:length]
		u32 := func(index int) uint32 {
			offset := procEventHeader + 4*index
			if offset+4 > len(event) {
				return 0
			}
			return order.Uint32(event[offset:])
		}
		switch order.Uint32(event[0:]) {
		case procEventNone:
			// The acknowledgement carries the subscription's error.
			if u32(0) == 0 {
				ack = max(ack, ackAccepted)
			} else {
				ack = ackRefused
			}
		case procEventFork:
			events = append(events, kernelEvent{Kind: kernelFork, Parent: int(u32(1)), PID: int(u32(2)), TGID: int(u32(3)), At: at})
		case procEventExec:
			events = append(events, kernelEvent{Kind: kernelExec, PID: int(u32(0)), TGID: int(u32(1)), At: at})
		case procEventExit:
			events = append(events, kernelEvent{Kind: kernelExit, PID: int(u32(0)), TGID: int(u32(1)), Status: u32(2), At: at})
		}
	}
	return events, ack
}

// procfs reads live processes from /proc. The helper is root outside the
// workload, so every workload process is readable.
type procfs struct{}

func (procfs) cgroup(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path, nil
		}
	}
	return "", execenv.ErrNotFound
}

func (procfs) parent(pid int) (int, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "PPid:"); ok {
			return strconv.Atoi(strings.TrimSpace(value))
		}
	}
	return 0, execenv.ErrNotFound
}

func (procfs) cmdline(pid int) ([]byte, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, io.LimitReader(f, cmdlineReadBytes)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (procfs) cwd(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
}
