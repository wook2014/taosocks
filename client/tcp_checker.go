package main

import (
	"container/list"
	"crypto/tls"
	"net"
	"sync"
	"time"
)

type _TcpCheckContext struct {
	wg *sync.WaitGroup
	ok bool
}

// TCPChecker is a synchronous TCP connectivity checker.
type TCPChecker struct {
	lock sync.Mutex
	maps map[string]*list.List
}

// NewTCPChecker news a TCP checker.
func NewTCPChecker() *TCPChecker {
	tc := &TCPChecker{}
	tc.maps = make(map[string]*list.List)
	return tc
}

// Check returns true if a TCP connection can be correctly made.
func (t *TCPChecker) Check(host, port string) bool {
	hostport := net.JoinHostPort(host, port)
	t.lock.Lock()
	var lst *list.List
	if l, ok := t.maps[hostport]; ok {
		lst = l
	} else {
		lst = list.New()
		t.maps[hostport] = lst
		go t.check(host, port)
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	ctx := &_TcpCheckContext{wg: wg}
	lst.PushBack(ctx)
	t.lock.Unlock()
	wg.Wait()
	return ctx.ok
}

func (t *TCPChecker) check(host, port string) (ok bool) {
	hostport := net.JoinHostPort(host, port)
	defer func() {
		t.finish(hostport, ok)
	}()
	switch port {
	case "443":
		return t.checkTLS(hostport)
	default:
		return t.checkTCP(hostport)
	}
}

func (t *TCPChecker) finish(hostport string, ok bool) {
	t.lock.Lock()
	defer t.lock.Unlock()
	lst := t.maps[hostport]
	for lst.Len() > 0 {
		elem := lst.Front()
		ctx := elem.Value.(*_TcpCheckContext)
		ctx.ok = ok
		ctx.wg.Done()
		lst.Remove(elem)
	}
	delete(t.maps, hostport)
}

func (t *TCPChecker) checkTCP(hostport string) bool {
	conn, err := net.DialTimeout("tcp4", hostport, time.Second*10)
	if err != nil {
		tslog.Red("? net.DialTimeout error: %s: %s", hostport, err)
		return false
	}
	defer conn.Close()
	return true
}

func (t *TCPChecker) checkTLS(hostport string) bool {
	conn, err := net.DialTimeout("tcp4", hostport, time.Second*10)
	if err != nil {
		tslog.Red("? net.DialTimeout error: %s: %s", hostport, err)
		return false
	}
	host, _, _ := net.SplitHostPort(hostport)
	tlsClient := tls.Client(conn, &tls.Config{ServerName: host})
	err = tlsClient.Handshake()
	tlsClient.Close()
	if err != nil {
		tslog.Red("? tls handshake error: %s: %s", hostport, err)
		return false
	}
	return true
}