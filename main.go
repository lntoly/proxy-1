package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/funny/crypto/aes256cbc"
	"github.com/funny/reuseport"
)

const miniBufferSize = 1024

var (
	cfgSecret      []byte
	cfgAddr        = "0.0.0.0:0"
	cfgReusePort   = false
	cfgDialRetry   = 1
	cfgDialTimeout = 3 * time.Second
	cfgBufferSize  = 8 * 1024

	codeOK          = []byte("200")
	codeBadReq      = []byte("400")
	codeBadAddr     = []byte("401")
	codeDialErr     = []byte("502")
	codeDialTimeout = []byte("504")

	isTest      bool
	gatewayAddr string
	bufferPool  sync.Pool
)

func main() {
	pid := syscall.Getpid()
	if err := ioutil.WriteFile("gateway.pid", []byte(strconv.Itoa(pid)), 0644); err != nil {
		log.Fatalf("Can't write pid file: %s", err)
	}
	defer os.Remove("gateway.pid")

	config()
	start()

	sigTERM := make(chan os.Signal, 1)
	signal.Notify(sigTERM, syscall.SIGTERM)
	printf("Gateway running, pid = %d", pid)
	<-sigTERM
	printf("Gateway killed")
}

func fatal(t string) {
	if !isTest {
		log.Fatal(t)
	}
	panic(t)
}

func fatalf(t string, args ...interface{}) {
	if !isTest {
		log.Fatalf(t, args...)
	}
	panic(fmt.Sprintf(t, args...))
}

func printf(t string, args ...interface{}) {
	if !isTest {
		log.Printf(t, args...)
	}
}

func config() {
	if cfgSecret = []byte(os.Getenv("GW_SECRET")); len(cfgSecret) == 0 {
		fatal("GW_SECRET is required")
	}
	printf("GW_SECRET=%s", cfgSecret)

	if cfgAddr = os.Getenv("GW_ADDR"); cfgAddr == "" {
		cfgAddr = "0.0.0.0:0"
	}
	printf("GW_ADDR=%s", cfgAddr)

	cfgReusePort = os.Getenv("GW_REUSE_PORT") == "1"

	var err error

	if v := os.Getenv("GW_DIAL_RETRY"); v != "" {
		cfgDialRetry, err = strconv.Atoi(v)
		if err != nil {
			fatalf("GW_DIAL_RETRY - %s", err)
		}
		if cfgDialRetry == 0 {
			cfgDialRetry = 1
		}
	}
	printf("GW_DIAL_RETRY=%d", cfgDialRetry)

	var timeout int
	if v := os.Getenv("GW_DIAL_TIMEOUT"); v != "" {
		timeout, err = strconv.Atoi(v)
		if err != nil {
			fatalf("GW_DIAL_TIMEOUT - %s", err)
		}
	}
	if timeout == 0 {
		timeout = 3
	}
	cfgDialTimeout = time.Duration(timeout) * time.Second
	printf("GW_DIAL_TIMEOUT=%d", timeout)

	if v := os.Getenv("GW_PPROF_ADDR"); v != "" {
		listener, err := net.Listen("tcp", v)
		if err != nil {
			fatalf("Setup pprof failed: %s", err)
		}
		printf("Setup pprof at %s", listener.Addr())
		go http.Serve(listener, nil)
	}

	if v := os.Getenv("GW_BUFF_SIZE"); v != "" {
		cfgBufferSize, err = strconv.Atoi(v)
		if err != nil {
			fatalf("GW_BUFF_SIZE - %s", err)
		}
		if cfgBufferSize < miniBufferSize {
			cfgBufferSize = miniBufferSize
		}
	}
	printf("GW_BUFF_SIZE=%d", cfgBufferSize)
	bufferPool.New = func() interface{} {
		return make([]byte, cfgBufferSize)
	}
}

func start() {
	var err error
	var listener net.Listener

	if cfgReusePort {
		listener, err = reuseport.NewReusablePortListener("tcp4", cfgAddr)
	} else {
		listener, err = net.Listen("tcp", cfgAddr)
	}
	if err != nil {
		fatalf("Setup listener failed: %s", err)
	}

	gatewayAddr = listener.Addr().String()
	printf("Setup gateway at %s", gatewayAddr)
	go loop(listener)
}

func loop(listener net.Listener) {
	defer listener.Close()
	for {
		conn, err := accept(listener)
		if err != nil {
			fatalf("Gateway accept failed: %s", err)
			return
		}
		go handle(conn)
	}
}

func accept(listener net.Listener) (net.Conn, error) {
	var tempDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				time.Sleep(tempDelay)
				continue
			}
			return nil, err
		}
		tempDelay = 0
		return conn, nil
	}
}

func handle(conn net.Conn) {
	defer func() {
		conn.Close()
		if err := recover(); err != nil {
			printf("Unhandled panic in connection handler: %v\n\n%s", err, debug.Stack())
		}
	}()

	agent := handshake(conn)
	if agent == nil {
		return
	}
	defer agent.Close()

	go func() {
		defer func() {
			agent.Close()
			conn.Close()
			if err := recover(); err != nil {
				printf("Unhandled panic in connection handler: %v\n\n%s", err, debug.Stack())
			}
		}()
		copy(conn, agent)
	}()
	copy(agent, conn)
}

func handshake(conn net.Conn) (agent net.Conn) {
	var addr []byte
	var remain []byte

	// read and decrypt target server address
	var buf [256]byte
	var err error
	for n, nn := 0, 0; n < len(buf); n += nn {
		nn, err = conn.Read(buf[n:])
		if err != nil {
			conn.Write(codeBadReq)
			return
		}
		if i := bytes.IndexByte(buf[n:n+nn], '\n'); i >= 0 {
			if addr, err = aes256cbc.DecryptBase64(cfgSecret, buf[:n+i]); err != nil {
				conn.Write(codeBadAddr)
				return nil
			}
			remain = buf[n+i+1 : n+nn]
			break
		}
	}
	if addr == nil {
		conn.Write(codeBadReq)
		return nil
	}

	// dial to target server
	for i := 0; i < cfgDialRetry; i++ {
		agent, err = net.DialTimeout("tcp", string(addr), cfgDialTimeout)
		if err == nil {
			break
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			continue
		}
		conn.Write(codeDialErr)
		return nil
	}
	if err != nil {
		conn.Write(codeDialTimeout)
		return nil
	}

	// send succeed code
	if _, err = conn.Write(codeOK); err != nil {
		agent.Close()
		return nil
	}

	// send remainder data in buffer
	if len(remain) > 0 {
		if _, err = agent.Write(remain); err != nil {
			agent.Close()
			return nil
		}
	}
	return
}
