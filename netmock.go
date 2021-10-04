// Package netmock is a mock implementation of a subset of the net package.
//
// netmock simulates TCP-like (i.e., reliable, ordered, error-checked)
// communication between hosts on a network. Additionally, netmock allows for
// simulating characteristics of real-world networks such as link latency and
// network partitions. Payload data is transferred via in-memory buffers in a
// thread-safe manner.
//
// Hosts are added to a network using the AddHost method. A host belongs to
// exactly one network (the network that it was created on). Within that
// network, the host is uniquely identified by one or more addresses assigned at
// creation. Addresses are not constrained to IP addresses, but can be arbitrary
// names consisting from letters (a-z, A-Z), digits (0-9), hyphens (-), colons
// (:), and periods (.). The first address from the slice of assigned addresses
// is selected as the host's "canonical" address. This address is used to
// identify the host when opening new connections to other hosts. Note that, as
// of now, netmock has no concept of ports.
//
//  nw :=  netmock.NewNetwork()
//
//  host, err:= nw.AddHost("localhost:8080")
//  if err != nil {
//      // handle error (address already assigned, invalid address, etc.)
//  }
//
//  // Hosts can have multiple addresses. The first address is picked as the canonical address.
//  dwarves, err := nw.AddHost("Doc", "Grumpy", "Happy", "Sleepy", "Bashful". "Sneezy", "Dopey")
//  dwarves.Addr() // "Doc"
//
// Connections between hosts are established using functions analogous to those
// found in the net package: Listen(), Dial(), and DialContext().
//
//  // server goroutine
//  ln, err := h1.Listen("localhost:8080")
//  if err != nil {
//      // handle error
//  }
//
//  for {
//      conn, err := ln.Accept()
//      if err != nil {
//          // handle error
//      }
//      go handleConnection(conn)
//  }
//
//  // client goroutine
//  conn, err := h2.Dial("localhost:8080")
//  if err != nil {
//      // handle error
//  }
//
// Delays between hosts can be configured on an address-to-address basis using
// the network's SetDelay method. The delay is applied to all existing and
// future connections between the specified addresses until a new delay is
// specified. As of now, delays are always symmetric.
//
//  n.SetDelay("localhost:8080", "localhost:8081", 100 * time.Millisecond)
//
// Hosts can be disconnected from the network using the Disconnect method.
// Existing connections will remain intact (i.e., are not closed), but will be
// unable to send or receive data. Similarly, dialing the disconnected host will
// result in an error. A disconnected host can be re-connected to the network
// using the Connect method.
//
// Delays are always incurred on the reader side. Writing to a conn is
// guaranteed to  succeed immediately, provided that the underlying buffer has
// sufficient capacity and the conn has not been closed. This mimics the
// behavior of TCP connections, where outgoing data is buffered by the OS before
// being sent over the network. From this perspective, a disconnected host
// experiences an infinite delay (until being reconnected to the network).
package netmock

import (
	"context"
	"net"
	"regexp"
	"sync"
	"time"
)

// DefaultBufSize is the default size (in bytes) of the internal buffer created
// for new connections between hosts.
const DefaultBufSize int = 16384

// A Network is a collection of hosts.
//
// Multiple goroutines may invoke methods on a Network simultaneously.
type Network struct {
	mu sync.Mutex

	// hosts is a map of addresses to hosts on the network.
	hosts map[string]*Host
}

// NewNetwork creates a new network with no hosts.
func NewNetwork() *Network {
	return &Network{
		hosts: make(map[string]*Host),
	}
}

// AddHost adds a new host to the network.
//
// The host is identified by the addresses specified in addrs. If addrs is
// empty or nil, or any of the addresses is invalid or already assigned, AddHost
// returns an error.
//
// Hosts created by AddHost are connected by default.
func (n *Network) AddHost(addrs ...string) (*Host, error) {
	if len(addrs) == 0 || addrs == nil {
		return nil, ErrNoAddrs
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	for _, addr := range addrs {
		if !validateAddr(addr) {
			return nil, ErrInvalidAddr
		}

		if _, ok := n.hosts[addr]; ok {
			return nil, ErrAddrInUse
		}
	}

	host := newHost(n, addrs...)
	for _, addr := range addrs {
		n.hosts[addr] = host
	}

	return host, nil
}

// SetDelay configures symmetric delays between addresses.
func (n *Network) SetDelay(fromAddr string, toAddr string, delay time.Duration) error {
	h1, h2 := n.Lookup(fromAddr), n.Lookup(toAddr)

	if h1 == nil || h2 == nil {
		return ErrUnknownAddr
	}

	h1.setDelay(toAddr, delay)
	h2.setDelay(fromAddr, delay)
	return nil
}

// Lookup returns the host that the given address is assigned to, or nil if
// the address is not assigned to any host.
func (n *Network) Lookup(address string) *Host {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hosts[address]
}

// A Host represents a member of a network.
//
// Multiple goroutines may invoke methods on a Host simultaneously.
type Host struct {
	mu sync.Mutex

	// nw is a reference to the network this host is part of
	nw *Network

	// addr is the canonical address of this host
	addr netAddr

	// connected indicates whether this host is connected to the network/
	connected bool

	// addrs is the set of addresses assigned to this host.
	listeners map[string]*listener

	// conns is the set of connections opened on this host (keyed by the remote
	// address).
	conns map[string](map[*conn]struct{})

	// delays is the set of delays introduced on connections between this host
	// and other hosts (keyed by the remote address).
	delays map[string]time.Duration

	// bufSize is the buffer size used when dialing new connections (defaults
	// to DefaultBufSize).
	bufSize int
}

// newHost creates a new host with the given addresses.
func newHost(nw *Network, addrs ...string) *Host {
	h := &Host{
		nw:        nw,
		connected: true,
		listeners: make(map[string]*listener),
		conns:     make(map[string]map[*conn]struct{}),
		delays:    make(map[string]time.Duration),
		addr:      netAddr(addrs[0]),
		bufSize:   DefaultBufSize,
	}

	for _, addr := range addrs {
		h.listeners[addr] = nil
	}

	return h
}

// Addr returns the canonical address of this host.
func (h *Host) Addr() net.Addr {
	return h.addr
}

// SetBufferSize sets the size (in bytes) of the internal buffers created for
// new connections.
//
// If the specified size is less than or equal to 0, the default buffer size is
// used.
func (h *Host) SetBufferSize(size int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if size <= 0 {
		h.bufSize = DefaultBufSize
	} else {
		h.bufSize = size
	}
}

// Connect connects this host to the network.
//
// Reconnecting a disconnected host will reverse the effects invoked by
// Disconnect. In particular, connections involving this host will resume
// operation, listeners will be able to accept new connections, and new
// connections can be dialed.
//
// This method is idempotent; it has no effect if the host is already connected.
func (h *Host) Connect() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.connected {
		h.connected = true

		for _, cc := range h.conns {
			for c := range cc {
				c.setState(true)
			}
		}
	}
}

// Disconnect disconnects this host from the network.
//
// When called on a connected host, all existing connections involving the host
// will stop operation: write operations will continue to succeed until the
// underlying buffer is full, whereas read operations will block until both
// peers are reconnected. This affects either peer (i.e., this host and the
// remote host). Attempts to establish new connections involving this host will
// fail until the host is reconnected. Similarly, calling Accept on a listener
// created by this host will block as long as the host remains disconnected.
//
// This method is idempotent; it has no effect if the host is already
// disconnected.
func (h *Host) Disconnect() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.connected {
		h.connected = false

		for _, cc := range h.conns {
			for c := range cc {
				c.setState(false)
			}
		}
	}
}

// Listen announces on the given address.
//
// If the address is not assigned to the host, or is already bound to another
// listener, an erorr is returend. Note that, in contrast to net.Listen, Listen
// will not bind to all available addresses when an empty address is specified.
func (h *Host) Listen(address string) (net.Listener, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	lis, ok := h.listeners[address]
	if !ok {
		return nil, ErrUnknownAddr
	}
	if lis != nil {
		return nil, ErrAddrInUse
	}

	lis = &listener{
		addr:        netAddr(address),
		doneCh:      make(chan struct{}),
		acceptCh:    make(chan *conn),
		onAccept:    func(c *conn) { h.registerConn(c) },
		onConnClose: func(c *conn) { h.unregisterConn(c) },
		onClose:     func() { h.unbindAddr(address) },
	}

	h.listeners[address] = lis
	return lis, nil
}

// Dial connects to the given address.
//
// Calling dial on a disconnected host will return an error. Similarly,
// specifying an invalid or unassigned address, or an adress that refers to a
// disconnected target host will also return an error.
//
// To specify a timeout or cancel dialling preemptively, use DialContext.
func (h *Host) Dial(address string) (net.Conn, error) {
	return h.DialContext(context.Background(), address)
}

func (h *Host) DialContext(ctx context.Context, address string) (net.Conn, error) {
	if !validateAddr(address) {
		return nil, ErrInvalidAddr
	}

	if !h.connected {
		return nil, ErrNoRoute
	}

	peer := h.nw.Lookup(address)
	c, err := peer.doDial(ctx, address, h, h.bufSize)
	if c != nil {
		h.registerConn(c)
		c.onClose = func() { h.unregisterConn(c) }
	}

	return c, err
}

func (h *Host) doDial(ctx context.Context, address string, remote *Host, bufsize int) (*conn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.connected {
		return nil, ErrNoRoute
	}

	lis := h.listeners[address]
	if lis == nil {
		return nil, ErrConnRefused
	}

	return lis.doDial(ctx, remote, bufsize)
}

func (h *Host) registerConn(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	addr := c.RemoteAddr().String()
	conns, ok := h.conns[addr]

	if !ok {
		conns = make(map[*conn]struct{})
		h.conns[addr] = conns
	}

	conns[c] = struct{}{}

	c.setState(h.connected)
	c.setDelay(h.delays[addr])
}

func (h *Host) unregisterConn(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	addr := c.RemoteAddr().String()
	conns := h.conns[addr]

	delete(conns, c)
	if len(conns) == 0 {
		delete(h.conns, addr)
	}
}

func (h *Host) unbindAddr(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listeners[addr] = nil
}

func (h *Host) setDelay(addr string, delay time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.delays[addr] = delay

	// update delay for existing connections
	if conns, ok := h.conns[addr]; ok {
		for c := range conns {
			c.setDelay(delay)
		}
	}
}

// Taken from google.golang.com/test/bufconn with modification
type listener struct {
	mu sync.Mutex

	// doneCh is a channel that is closed when Close() is called initially
	doneCh chan struct{}

	acceptCh chan *conn

	onAccept    func(*conn)
	onConnClose func(*conn)
	onClose     func()

	// addr is the address that this listener is bound to.
	addr netAddr
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case <-l.doneCh:
		return nil, ErrConnRefused
	case c := <-l.acceptCh:
		l.onAccept(c)
		c.onClose = func() { l.onConnClose(c) }
		return c, nil
	}
}

func (l *listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	select {
	case <-l.doneCh:
		// already closed
		break
	default:
		close(l.doneCh)
		l.onClose()
	}

	return nil
}

func (l *listener) Addr() net.Addr {
	return l.addr
}

// doDial creates a new connection between the remote host and the listener.
func (l *listener) doDial(ctx context.Context, remote *Host, bufsize int) (*conn, error) {
	p1, p2 := newPipe(bufsize), newPipe(bufsize)

	// lconn, rconn are the local and remote conns of the established connection
	// from the perspective of this host (i.e. the called host)
	lconn := &conn{Reader: p1, Writer: p2, laddr: l.addr, raddr: remote.addr}
	rconn := &conn{Reader: p2, Writer: p1, laddr: remote.addr, raddr: l.addr}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()

	case <-l.doneCh:
		return nil, ErrConnRefused

	case l.acceptCh <- lconn:
		return rconn, nil
	}
}

// A netAddr is an address compatible with net.Addr.
type netAddr string

func (a netAddr) Network() string { return "netmock" }
func (a netAddr) String() string  { return string(a) }

// addrRegex is a regular expression that matches valid network addresses.
var addrRegex = regexp.MustCompile("^[a-zA-Z0-9_.:-]*$")

func validateAddr(addr string) bool {
	return addrRegex.MatchString(addr)
}
