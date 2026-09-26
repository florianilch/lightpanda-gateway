package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		run()
	case "serve":
		serve()
	default:
		os.Exit(2)
	}
}

func run() {
	if len(os.Args) < 3 {
		os.Exit(2)
	}
	script, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := fmt.Fprintf(os.Stdout, "fake-lightpanda:%s", script); err != nil {
		os.Exit(2)
	}
}

func serve() {
	host := argValue("--host")
	port := argValue("--port")
	if host == "" || port == "" {
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer ln.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	fmt.Fprintf(os.Stderr, "$time=%d $scope=app $level=note $msg=\"server running\" address=%s\n", time.Now().UnixMilli(), ln.Addr())
	conn, err := ln.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = req.Body.Close()
	if req.Method != http.MethodGet || req.URL.RequestURI() != "/" || req.Proto != "HTTP/1.1" ||
		!strings.EqualFold(req.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(req.Header.Get("Upgrade"), "websocket") ||
		req.Header.Get("Sec-WebSocket-Version") != "13" ||
		req.Header.Get("Sec-WebSocket-Key") != "dGhlIHNhbXBsZSBub25jZQ==" {
		os.Exit(2)
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n"); err != nil {
		os.Exit(2)
	}

	payload := make([]byte, len("smoke-cdp"))
	if _, err := io.ReadFull(br, payload); err != nil {
		if ctx.Err() != nil {
			return
		}
		os.Exit(2)
	}
	if _, err := io.WriteString(conn, "fake-cdp!"); err != nil {
		os.Exit(2)
	}
	// Stay alive after the tunnel closes. Gateway should stop the browser process.
	<-ctx.Done()
}

func argValue(name string) string {
	for i := 2; i+1 < len(os.Args); i++ {
		if os.Args[i] == name {
			return os.Args[i+1]
		}
	}
	return ""
}
