//go:build p3accacceptance

package main

import (
	"context"
	"flag"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const p3ACCRelayMaximumConnections = 128

func main() {
	flags := flag.NewFlagSet("p3accproxy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listenValue := flags.String("listen", "", "")
	upstreamValue := flags.String("upstream", "", "")
	announcePort := flags.Bool("announce-port", false, "")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 {
		os.Exit(2)
	}
	listenAddress, listenErr := p3ACCRelayLoopbackAddress(*listenValue, true)
	upstreamAddress, upstreamErr := p3ACCRelayLoopbackAddress(*upstreamValue, false)
	listenEndpoint, endpointErr := netip.ParseAddrPort(listenAddress)
	if listenErr != nil || upstreamErr != nil || endpointErr != nil ||
		(listenEndpoint.Port() == 0 && !*announcePort) {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listenAddress)
	if err != nil {
		os.Exit(3)
	}
	if *announcePort && p3ACCAnnounceRelayPort(os.Stdout, listener.Addr()) != nil {
		_ = listener.Close()
		os.Exit(3)
	}
	if err := p3ACCServeRelay(ctx, listener, upstreamAddress); err != nil {
		os.Exit(3)
	}
}

func p3ACCRelayLoopbackAddress(value string, allowUnspecifiedPort bool) (string, error) {
	address, err := netip.ParseAddrPort(value)
	if err != nil || !address.Addr().IsLoopback() ||
		(!allowUnspecifiedPort && address.Port() == 0) {
		return "", errP3ACCRelayAddress
	}
	return address.String(), nil
}

func p3ACCAnnounceRelayPort(output io.Writer, address net.Addr) error {
	if output == nil || address == nil {
		return errP3ACCRelayAnnouncement
	}
	tcpAddress, ok := address.(*net.TCPAddr)
	if !ok || tcpAddress.Port < 1 || tcpAddress.Port > 65535 ||
		tcpAddress.IP == nil || !tcpAddress.IP.IsLoopback() {
		return errP3ACCRelayAnnouncement
	}
	payload := []byte(strconv.Itoa(tcpAddress.Port) + "\n")
	written, err := output.Write(payload)
	if err != nil || written != len(payload) {
		return errP3ACCRelayAnnouncement
	}
	return nil
}

func p3ACCServeRelay(ctx context.Context, listener net.Listener, upstreamAddress string) error {
	if ctx == nil || listener == nil {
		return errP3ACCRelayConfiguration
	}
	if _, err := p3ACCRelayLoopbackAddress(upstreamAddress, false); err != nil {
		_ = listener.Close()
		return err
	}

	connections := make(map[net.Conn]struct{})
	var connectionsMu sync.Mutex
	var workers sync.WaitGroup
	capacity := make(chan struct{}, p3ACCRelayMaximumConnections)
	done := make(chan struct{})
	closeConnections := func() {
		connectionsMu.Lock()
		defer connectionsMu.Unlock()
		for connection := range connections {
			_ = connection.Close()
		}
	}
	defer func() {
		close(done)
		_ = listener.Close()
		closeConnections()
		workers.Wait()
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
			closeConnections()
		case <-done:
		}
	}()

	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errP3ACCRelayAccept
		}
		select {
		case capacity <- struct{}{}:
		case <-ctx.Done():
			_ = client.Close()
			return nil
		default:
			_ = client.Close()
			continue
		}
		connectionsMu.Lock()
		connections[client] = struct{}{}
		connectionsMu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-capacity }()
			defer func() {
				connectionsMu.Lock()
				delete(connections, client)
				connectionsMu.Unlock()
				_ = client.Close()
			}()
			p3ACCRelayConnection(ctx, client, upstreamAddress, &connectionsMu, connections)
		}()
	}
}

func p3ACCRelayConnection(
	ctx context.Context,
	client net.Conn,
	upstreamAddress string,
	connectionsMu *sync.Mutex,
	connections map[net.Conn]struct{},
) {
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	upstream, err := dialer.DialContext(ctx, "tcp", upstreamAddress)
	if err != nil {
		return
	}
	connectionsMu.Lock()
	connections[upstream] = struct{}{}
	connectionsMu.Unlock()
	defer func() {
		connectionsMu.Lock()
		delete(connections, upstream)
		connectionsMu.Unlock()
		_ = upstream.Close()
	}()

	finished := make(chan struct{}, 2)
	var copies sync.WaitGroup
	copies.Add(2)
	copyStream := func(destination, source net.Conn) {
		defer copies.Done()
		_, _ = io.Copy(destination, source)
		finished <- struct{}{}
	}
	go copyStream(upstream, client)
	go copyStream(client, upstream)
	select {
	case <-ctx.Done():
	case <-finished:
	}
	_ = client.Close()
	_ = upstream.Close()
	copies.Wait()
}
