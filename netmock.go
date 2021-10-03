package netmock

import (
	"context"
	"log"
	"net"
	"regexp"
	"sync"
	"time"
)

// A netAddr is an address compatible with net.Addr.
type netAddr string

func (a netAddr) Network() string { return "netmock" }
func (a netAddr) String() string  { return string(a) }

// addrRegex is a regular expression that matches valid network addresses.
var addrRegex = regexp.MustCompile("^[a-zA-Z0-9_.:-]*$")

func validateAddr(addr string) bool {
	return addrRegex.MatchString(addr)
}

const DefaultBufSize int = 16384

// A network is a set of hosts identified by one or more addresses.
type Network struct {
	mu    sync.Mutex
	hosts map[string]*Host
}

func NewNetwork() *Network {
	return &Network{
		hosts: make(map[string]*Host),
	}
}

func (n *Network) AddHost(addrs ...string) (*Host, error) {
	if len(addrs) == 0 {
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

func (n *Network) SetDelay(fromAddr string, toAddr string, delay time.Duration) error {
	h1, h2 := n.Resolve(fromAddr), n.Resolve(toAddr)

	if h1 == nil || h2 == nil {
		return ErrUnknownAddr
	}

	h1.setDelay(toAddr, delay)
	h2.setDelay(fromAddr, delay)
	return nil
}

// Resolve returns the host that the given address is assigned to, or nil.
func (n *Network) Resolve(addr string) *Host {
	n.mu.Lock()
	defer n.mu.Unlock()
	log.Println(n.hosts)
	return n.hosts[addr]
}

type Host struct {
	mu sync.Mutex

	// nw is a reference to the network this host is part of
	nw *Network

	// addr is the canonical address of this host
	addr netAddr

	// connected indicates whether this host is connected to the network
	connected bool

	// addrs is the set of addresses assigned to this host
	listeners map[string]*listener

	// conns is the set of connections opened on this host (keyed by the remote
	// address)
	conns map[string](map[*conn]struct{})

	// delays is the set of delays between this host and other hosts
	delays map[string]time.Duration

	// bufSize is the buffer size used when dialing new connections
	bufSize int
}

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

func (h *Host) Addr() net.Addr {
	return h.addr
}

func (h *Host) SetBufferSize(size int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bufSize = size
}

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

// listen returns a new listener on the given address, or error if another
// listener is already bound to the address or the address is not assigned to
// this host.
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

	peer := h.nw.Resolve(address)
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
