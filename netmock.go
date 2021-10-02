package netmock

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// A netErrorTimeout is an error that indicates a timeout and is compatible
// with net.Error.
type netErrorTimeout struct{ error }

func (e netErrorTimeout) Timeout() bool   { return true }
func (e netErrorTimeout) Temporary() bool { return false }

var ErrTimeout netErrorTimeout = netErrorTimeout{fmt.Errorf("timeout")}

// Taken from google.golang.com/test/bufconn with modification
// A pipe is a unidirectional read/write channel. It represents one half of
// a fully-duplex TCP connection.
type pipe struct {
	mu sync.Mutex

	// buf contains the data in the pipe.  It is a ring buffer of fixed capacity,
	// with r and w pointing to the offset to read and write, respsectively.
	//
	// Data is read between [r, w) and written to [w, r), wrapping around the end
	// of the slice if necessary.
	//
	// The buffer is empty if r == len(buf), otherwise if r == w, it is full.
	//
	// w and r are always in the range [0, cap(buf)) and [0, len(buf)].
	buf  []byte
	w, r int

	wwait sync.Cond
	rwait sync.Cond

	wtimedout bool
	rtimedout bool

	wtimer *time.Timer
	rtimer *time.Timer

	wconnected bool          // true if writer is connected
	rconnected bool          // true if reader is connected
	cwait      sync.Cond     // wait for both peers to connect
	delay      time.Duration // delay for read operation

	closed      bool
	writeClosed bool
}

func newPipe(sz int) *pipe {
	p := &pipe{buf: make([]byte, 0, sz)}
	p.wwait.L = &p.mu
	p.rwait.L = &p.mu
	p.cwait.L = &p.mu

	p.wtimer = time.AfterFunc(0, func() {})
	p.rtimer = time.AfterFunc(0, func() {})
	return p
}

func (p *pipe) empty() bool {
	return p.r == len(p.buf)
}

func (p *pipe) full() bool {
	return p.r < len(p.buf) && p.r == p.w
}

func (p *pipe) Read(b []byte) (n int, err error) {
	p.mu.Lock()

	// Block until p has data.
	for {
		if p.closed {
			return 0, io.ErrClosedPipe
		}
		if !p.rconnected || !p.wconnected {
			p.cwait.Wait()
		}
		if !p.empty() {
			break
		}
		if p.writeClosed {
			return 0, io.EOF
		}
		if p.rtimedout {
			// TODO errors
			return 0, ErrTimeout
		}

		p.rwait.Wait()
	}
	wasFull := p.full()

	n = copy(b, p.buf[p.r:len(p.buf)])
	p.r += n
	if p.r == cap(p.buf) {
		p.r = 0
		p.buf = p.buf[:p.w]
	}

	// Signal a blocked writer, if any
	if wasFull {
		p.wwait.Signal()
	}

	// The lock needs to be released before the delay is executed to avoid
	// effecting concurrent writes.
	d := p.delay
	p.mu.Unlock()

	// Sleep for the specified delay
	time.Sleep(d)

	return n, nil
}

func (p *pipe) Write(b []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	for len(b) > 0 {
		// Block until p is not full.
		for {
			if p.closed || p.writeClosed {
				return 0, io.ErrClosedPipe
			}
			if !p.full() {
				break
			}
			if p.wtimedout {
				return 0, ErrTimeout
			}

			p.wwait.Wait()
		}
		wasEmpty := p.empty()

		end := cap(p.buf)
		if p.w < p.r {
			end = p.r
		}
		x := copy(p.buf[p.w:end], b)
		b = b[x:]
		n += x
		p.w += x
		if p.w > len(p.buf) {
			p.buf = p.buf[:p.w]
		}
		if p.w == cap(p.buf) {
			p.w = 0
		}

		// Signal a blocked reader, if any.
		if wasEmpty {
			p.rwait.Signal()
		}
	}
	return n, nil
}

func (p *pipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	// Signal all blocked readers and writers to return an error.
	p.rwait.Broadcast()
	p.wwait.Broadcast()
	return nil
}

func (p *pipe) closeWrite() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeClosed = true
	// Signal all blocked readers and writers to return an error.
	p.rwait.Broadcast()
	p.wwait.Broadcast()
	return nil
}

func (p *pipe) setWriteDeadline(t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.wtimer.Stop()
	p.wtimedout = false

	if !t.IsZero() {
		p.wtimer = time.AfterFunc(time.Until(t), func() {
			p.mu.Lock()
			defer p.mu.Unlock()

			p.wtimedout = true
			p.wwait.Broadcast()
		})
	}
}

func (p *pipe) setReadDeadline(t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.rtimer.Stop()
	p.rtimedout = false

	if !t.IsZero() {
		p.rtimer = time.AfterFunc(time.Until(t), func() {
			p.mu.Lock()
			defer p.mu.Unlock()

			p.rtimedout = true
			p.rwait.Broadcast()
		})
	}
}

func (p *pipe) SetReadConnection(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.rconnected = up
	if p.rconnected && p.wconnected {
		p.cwait.Broadcast()
	}
}

func (p *pipe) SetWriteConnection(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.wconnected = up
	if p.rconnected && p.wconnected {
		p.cwait.Broadcast()
	}
}

func (p *pipe) setDelay(t time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delay = t
}