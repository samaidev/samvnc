package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	target := flag.String("url", "wss://aitun.cc/IKG5FW3F/ws", "ws url")
	proxy := flag.String("proxy", "http://127.0.0.1:18080", "http proxy")
	origin := flag.String("origin", "https://aitun.cc", "Origin header")
	flag.Parse()

	d := &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: false},
	}
	if *proxy != "" {
		u, err := url.Parse(*proxy)
		if err != nil {
			log.Fatal(err)
		}
		d.Proxy = http.ProxyURL(u)
	}
	hdr := http.Header{}
	hdr.Set("Origin", *origin)
	hdr.Set("User-Agent", "samvnc-wstest/1.0")

	fmt.Fprintln(os.Stderr, "dialing", *target, "via", *proxy, "origin", *origin)
	c, resp, err := d.Dial(*target, hdr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "DIAL ERR:", err)
		if resp != nil {
			fmt.Fprintf(os.Stderr, "resp status: %s\n", resp.Status)
			fmt.Fprintln(os.Stderr, "resp headers:")
			resp.Header.Write(os.Stderr)
			buf := make([]byte, 4096)
			n, _ := resp.Body.Read(buf)
			fmt.Fprintf(os.Stderr, "resp body: %s\n", string(buf[:n]))
		}
		os.Exit(1)
	}
	defer c.Close()
	fmt.Fprintln(os.Stderr, "WS CONNECTED:", resp.Status)

	hello := `{"type":"hello","version":1,"quality":1,"fps":15,"mode":0}`
	if err := c.WriteMessage(websocket.TextMessage, []byte(hello)); err != nil {
		log.Fatal("write:", err)
	}
	fmt.Fprintln(os.Stderr, "sent hello")

	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	mt, data, err := c.ReadMessage()
	if err != nil {
		log.Fatal("read:", err)
	}
	fmt.Printf("RECV mt=%d len=%d\n", mt, len(data))
	if len(data) > 400 {
		fmt.Printf("preview: %s\n", string(data[:400]))
	} else {
		fmt.Printf("payload: %s\n", string(data))
	}
	for i := 0; i < 3; i++ {
		_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
		mt, data, err := c.ReadMessage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "read[%d] err: %v\n", i, err)
			break
		}
		fmt.Printf("RECV[%d] mt=%d len=%d\n", i, mt, len(data))
	}
}
