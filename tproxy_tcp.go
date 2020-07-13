// +build linux

package tproxy

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

// Listener describes a TCP Listener
// with the Linux IP_TRANSPARENT option defined
// on the listening socket
type Listener struct {
	base net.Listener
}

// Accept waits for and returns
// the next connection to the listener.
//
// This command wraps the AcceptTProxy
// method of the Listener
func (listener *Listener) Accept() (net.Conn, error) {
	return listener.AcceptTProxy()
}

// AcceptTProxy will accept a TCP connection
// and wrap it to a TProxy connection to provide
// TProxy functionality
func (listener *Listener) AcceptTProxy() (*Conn, error) {
	tcpConn, err := listener.base.(*net.TCPListener).AcceptTCP()
	if err != nil {
		return nil, err
	}

	return &Conn{TCPConn: tcpConn}, nil
}

// Addr returns the network address
// the listener is accepting connections
// from
func (listener *Listener) Addr() net.Addr {
	return listener.base.Addr()
}

// Close will close the listener from accepting
// any more connections. Any blocked connections
// will unblock and close
func (listener *Listener) Close() error {
	return listener.base.Close()
}

// ListenTCP will construct a new TCP listener
// socket with the Linux IP_TRANSPARENT option
// set on the underlying socket
func ListenTCP(network string, laddr *net.TCPAddr) (net.Listener, error) {
	listener, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	fileDescriptorSource, err := listener.File()
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("get file descriptor: %s", err)}
	}
	defer fileDescriptorSource.Close()

	// 增加SO_MARK：255
	// 配合iptables避免出现本程序发出的请求也被环回处理：iptables -t mangle -A OUTPUT -j RETURN -m mark --mark 0xff
	//if err := syscall.SetsockoptInt(int(fileDescriptorSource.Fd()), syscall.SOL_SOCKET, syscall.SO_MARK, 255); err != nil {
	//	return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: SO_MARK: %s", err)}
	//}

	if err = syscall.SetsockoptInt(int(fileDescriptorSource.Fd()), syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_TRANSPARENT: %s", err)}
	}

	return &Listener{listener}, nil
}

// Conn describes a connection
// accepted by the TProxy listener.
//
// It is simply a TCP connection with
// the ability to dial a connection to
// the original destination while assuming
// the IP address of the client
type Conn struct {
	*net.TCPConn
}

// DialOriginalDestination will open a
// TCP connection to the original destination
// that the client was trying to connect to before
// being intercepted.
//
// When `dontAssumeRemote` is false, the connection will
// originate from the IP address and port that the client
// used when making the connection. Otherwise, when true,
// the connection will originate from an IP address and port
// assigned by the Linux kernel that is owned by the
// operating system
func (conn *Conn) DialOriginalDestination(dontAssumeRemote bool) (*net.TCPConn, error) {
	remoteSocketAddress, err := tcpAddrToSocketAddr(conn.LocalAddr().(*net.TCPAddr))
	if err != nil {
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("build destination socket address: %s", err)}
	}

	localSocketAddress, err := tcpAddrToSocketAddr(conn.RemoteAddr().(*net.TCPAddr))
	if err != nil {
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("build local socket address: %s", err)}
	}

	fileDescriptor, err := syscall.Socket(tcpAddrFamily("tcp", conn.LocalAddr().(*net.TCPAddr), conn.RemoteAddr().(*net.TCPAddr)), syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	//fileDescriptor, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("socket open: %s", err)}
	}

	if err = syscall.SetsockoptInt(fileDescriptor, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fileDescriptor)
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("set socket option: SO_REUSEADDR: %s", err)}
	}

	// 增加SO_MARK：255
	// 配合iptables避免出现本程序发出的请求也被环回处理：iptables -t mangle -A OUTPUT -j RETURN -m mark --mark 0xff
	if err := syscall.SetsockoptInt(fileDescriptor, syscall.SOL_SOCKET, syscall.SO_MARK, 255); err != nil {
		//return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: SO_MARK: %s", err)}
		syscall.Close(fileDescriptor)
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("set socket option: SO_MARK: %s", err)}
	}

	if err = syscall.SetsockoptInt(fileDescriptor, syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); err != nil {
		syscall.Close(fileDescriptor)
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("set socket option: IP_TRANSPARENT: %s", err)}
	}

	if err = syscall.SetNonblock(fileDescriptor, true); err != nil {
		syscall.Close(fileDescriptor)
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("set socket option: SO_NONBLOCK: %s", err)}
	}

	if !dontAssumeRemote {
		if err = syscall.Bind(fileDescriptor, localSocketAddress); err != nil {
			syscall.Close(fileDescriptor)
			return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("socket bind: %s", err)}
		}
	}

	//if err = syscall.Connect(fileDescriptor, remoteSocketAddress); err != nil && !strings.Contains(err.Error(), "operation now in progress") {
	//	syscall.Close(fileDescriptor)
	//	return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("socket connect: %s", err)}
	//}
	if err = connect(fileDescriptor, remoteSocketAddress, time.Time{}); err != nil {
		syscall.Close(fileDescriptor)
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("socket connect: %s", err)}
	}

	fdFile := os.NewFile(uintptr(fileDescriptor), fmt.Sprintf("net-tcp-dial-%s", conn.LocalAddr().String()))
	defer fdFile.Close()

	remoteConn, err := net.FileConn(fdFile)
	if err != nil {
		syscall.Close(fileDescriptor)
		return nil, &net.OpError{Op: "dial", Err: fmt.Errorf("convert file descriptor to connection: %s", err)}
	}

	return remoteConn.(*net.TCPConn), nil
}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

var errTimeout = &timeoutError{}

func FD_SET(fd uintptr, p *syscall.FdSet) {
	n, k := fd/32, fd%32
	p.Bits[n] |= (1 << uint32(k))
}

// this is close to the connect() function inside stdlib/net
func connect(fd int, ra syscall.Sockaddr, deadline time.Time) error {
	switch err := syscall.Connect(fd, ra); err {
	case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
	case nil, syscall.EISCONN:
		if !deadline.IsZero() && deadline.Before(time.Now()) {
			return errTimeout
		}
		return nil
	default:
		return err
	}

	var err error
	var to syscall.Timeval
	var toptr *syscall.Timeval
	var pw syscall.FdSet
	FD_SET(uintptr(fd), &pw)
	for {
		// wait until the fd is ready to read or write.
		if !deadline.IsZero() {
			to = syscall.NsecToTimeval(deadline.Sub(time.Now()).Nanoseconds())
			toptr = &to
		}

		// wait until the fd is ready to write. we can't use:
		//   if err := fd.pd.WaitWrite(); err != nil {
		//   	 return err
		//   }
		// so we use select instead.
		if _, err = Select(fd+1, nil, &pw, nil, toptr); err != nil {
			fmt.Println(err)
			return err
		}

		var nerr int
		nerr, err = syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			return err
		}
		switch err = syscall.Errno(nerr); err {
		case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
			continue
		case syscall.Errno(0), syscall.EISCONN:
			if !deadline.IsZero() && deadline.Before(time.Now()) {
				return errTimeout
			}
			return nil
		default:
			return err
		}
	}
}

func Select(nfd int, r *syscall.FdSet, w *syscall.FdSet, e *syscall.FdSet, timeout *syscall.Timeval) (n int, err error) {
	return syscall.Select(nfd, r, w, e, timeout)
}

// tcpAddToSockerAddr will convert a TCPAddr
// into a Sockaddr that may be used when
// connecting and binding sockets
func tcpAddrToSocketAddr(addr *net.TCPAddr) (syscall.Sockaddr, error) {
	switch {
	case addr.IP.To4() != nil:
		ip := [4]byte{}
		copy(ip[:], addr.IP.To4())

		return &syscall.SockaddrInet4{Addr: ip, Port: addr.Port}, nil

	default:
		ip := [16]byte{}
		copy(ip[:], addr.IP.To16())

		zoneID, err := strconv.ParseUint(addr.Zone, 10, 32)
		if err != nil {
			return nil, err
		}

		return &syscall.SockaddrInet6{Addr: ip, Port: addr.Port, ZoneId: uint32(zoneID)}, nil
	}
}

// tcpAddrFamily will attempt to work
// out the address family based on the
// network and TCP addresses
func tcpAddrFamily(net string, laddr, raddr *net.TCPAddr) int {
	switch net[len(net)-1] {
	case '4':
		return syscall.AF_INET
	case '6':
		return syscall.AF_INET6
	}

	if (laddr == nil || laddr.IP.To4() != nil) &&
		(raddr == nil || laddr.IP.To4() != nil) {
		return syscall.AF_INET
	}
	return syscall.AF_INET6
}
