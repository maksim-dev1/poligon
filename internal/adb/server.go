package adb

import (
	"errors"
	"net"
	"sync/atomic"
	"time"
)

// ErrNoServer is returned instead of running adb while the farm's adb server
// is down.
var ErrNoServer = errors.New("adb server is not running (com.pancir.adb restarts it)")

// managedAddr, once set, is the address of an adb server that launchd owns.
var managedAddr atomic.Pointer[string]

// UseManagedServer tells every ADB in the process that the server at addr is
// owned by a service. From then on no command runs while it is down: the adb
// client would otherwise start a daemonized server of its own — one no service
// owns or ever stops, which is how a farm ends up with two servers fighting
// over the phones' USB.
func UseManagedServer(addr string) { managedAddr.Store(&addr) }

// ServerUp reports whether commands may run now. Always true when the server
// is not managed (a dev machine: the client starts one as usual).
func ServerUp() bool {
	p := managedAddr.Load()
	if p == nil {
		return true
	}
	c, err := net.DialTimeout("tcp", *p, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
