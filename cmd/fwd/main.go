// fwd is a tiny TCP forwarder that bridges a local listener to a remote
// host:port through an HTTP proxy's CONNECT method. It exists because the
// SamVNC sandbox only allows egress via an HTTP proxy, while the aitun.cc
// relay client dials its relay server (aitun.cc:6639) directly.
//
// Usage:
//   fwd -listen 127.0.0.1:6639 -proxy 127.0.0.1:18080 -target 43.160.208.156:6639
//
// Combined with `127.0.0.1 aitun.cc` in /etc/hosts, aitun connects to this
// forwarder (thinking it's aitun.cc:6639) and gets tunneled to the real relay
// with correct TLS SNI.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:6639", "local listen address")
	proxy := flag.String("proxy", "127.0.0.1:18080", "HTTP proxy address")
	target := flag.String("target", "43.160.208.156:6639", "remote host:port to CONNECT to")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}
	log.Printf("forwarder %s -> proxy %s -> %s", *listen, *proxy, *target)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(c, *proxy, *target)
	}
}

func handle(c net.Conn, proxy, target string) {
	defer c.Close()
	p, err := net.Dial("tcp", proxy)
	if err != nil {
		log.Printf("dial proxy: %v", err)
		return
	}
	defer p.Close()

	// Issue CONNECT.
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	if _, err := p.Write([]byte(req)); err != nil {
		log.Printf("write connect: %v", err)
		return
	}
	br := bufio.NewReader(p)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		log.Printf("read status: %v", err)
		return
	}
	if !strings.Contains(statusLine, " 200 ") {
		log.Printf("proxy refused CONNECT: %s", strings.TrimSpace(statusLine))
		return
	}
	// Drain response headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// Pipe bidirectionally. br may hold buffered bytes from the proxy, so use
	// it for the proxy->client direction.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(p, c) }()
	go func() { defer wg.Done(); io.Copy(c, br) }()
	wg.Wait()
}
