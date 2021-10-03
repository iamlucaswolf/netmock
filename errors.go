package netmock

import (
	"errors"
	"fmt"
)

// A netErrorTimeout is an error that indicates a timeout and is compatible
// with net.Error.
type netErrorTimeout struct{ error }

func (e netErrorTimeout) Timeout() bool   { return true }
func (e netErrorTimeout) Temporary() bool { return false }

var ErrTimeout netErrorTimeout = netErrorTimeout{fmt.Errorf("timeout")}

var ErrClosed = errors.New("closed")

var ErrInvalidAddr = errors.New("invalid address")

var ErrAddrInUse = errors.New("addr in use")

var ErrNoAddrs = errors.New("no addresses")

var ErrUnknownAddr = errors.New("unknown address")

var ErrNoRoute = errors.New("no route")

var ErrConnRefused = errors.New("connection refused")
