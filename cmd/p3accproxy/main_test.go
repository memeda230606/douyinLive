//go:build p3accacceptance

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type p3ACCShortWriter struct{}

func (p3ACCShortWriter) Write(payload []byte) (int, error) {
	return len(payload) - 1, nil
}

func TestP3ACCRelayLoopbackAddress(t *testing.T) {
	for _, test := range []struct {
		name        string
		value       string
		allowZero   bool
		wantFailure bool
	}{
		{name: "ipv4", value: "127.0.0.1:7890"},
		{name: "ipv6", value: "[::1]:7890"},
		{name: "listen dynamic", value: "127.0.0.1:0", allowZero: true},
		{name: "upstream zero", value: "127.0.0.1:0", wantFailure: true},
		{name: "all interfaces", value: "0.0.0.0:7890", wantFailure: true},
		{name: "remote", value: "192.0.2.1:7890", wantFailure: true},
		{name: "hostname", value: "localhost:7890", wantFailure: true},
		{name: "missing", value: "", wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := p3ACCRelayLoopbackAddress(test.value, test.allowZero)
			if (err != nil) != test.wantFailure {
				t.Fatalf("p3ACCRelayLoopbackAddress() error = %v", err)
			}
		})
	}
}

func TestP3ACCAnnounceRelayPort(t *testing.T) {
	var output bytes.Buffer
	if err := p3ACCAnnounceRelayPort(&output, &net.TCPAddr{
		IP: net.ParseIP("127.0.0.1"), Port: 54321,
	}); err != nil {
		t.Fatalf("p3ACCAnnounceRelayPort() error = %v", err)
	}
	if output.String() != "54321\n" {
		t.Fatalf("announcement = %q", output.String())
	}
	for _, address := range []net.Addr{
		nil,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0},
		&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 54321},
	} {
		if err := p3ACCAnnounceRelayPort(io.Discard, address); err == nil {
			t.Fatalf("invalid announcement address accepted: %#v", address)
		}
	}
	if err := p3ACCAnnounceRelayPort(p3ACCShortWriter{}, &net.TCPAddr{
		IP: net.ParseIP("127.0.0.1"), Port: 54321,
	}); err == nil {
		t.Fatal("short announcement write accepted")
	}
}

func TestP3ACCServeRelayForwardsAndStops(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer upstream.Close()
	upstreamDone := make(chan struct{})
	go func() {
		defer close(upstreamDone)
		connection, acceptErr := upstream.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen relay: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- p3ACCServeRelay(ctx, listener, upstream.Addr().String())
	}()

	client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		cancel()
		t.Fatalf("dial relay: %v", err)
	}
	defer client.Close()
	deadline := time.Now().Add(2 * time.Second)
	_ = client.SetDeadline(deadline)
	if _, err := client.Write([]byte("relay-probe")); err != nil {
		cancel()
		t.Fatalf("write relay: %v", err)
	}
	buffer := make([]byte, len("relay-probe"))
	if _, err := io.ReadFull(client, buffer); err != nil || string(buffer) != "relay-probe" {
		cancel()
		t.Fatalf("read relay = %q, %v", buffer, err)
	}

	cancel()
	select {
	case err := <-relayDone:
		if err != nil {
			t.Fatalf("relay shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("active client remained open after relay stop")
	}
	_ = upstream.Close()
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("upstream did not stop")
	}
}

func TestP3ACCServeRelayRejectsInvalidConfiguration(t *testing.T) {
	if err := p3ACCServeRelay(context.Background(), nil, "127.0.0.1:7890"); !errors.Is(err, errP3ACCRelayConfiguration) {
		t.Fatalf("nil listener error = %v", err)
	}
}
