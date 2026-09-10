package app

import (
	"net"
	"os"
	"strconv"
	"testing"
)

func TestActivatedListener(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	file, err := base.(*net.TCPListener).File()
	if err != nil {
		base.Close()
		t.Fatal(err)
	}
	base.Close()

	processID := os.Getpid()
	listener, inherited, err := activatedListener(processID, strconv.Itoa(processID), "1", file.Fd())
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !inherited {
		t.Fatal("listener was not inherited")
	}
	defer listener.Close()

	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			err = connection.Close()
		}
		accepted <- err
	}()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestActivatedListenerIgnoresAnotherProcess(t *testing.T) {
	listener, inherited, err := activatedListener(os.Getpid(), strconv.Itoa(os.Getpid()+1), "1", systemdListenFD)
	if err != nil {
		t.Fatal(err)
	}
	if listener != nil || inherited {
		t.Fatalf("listener = %v, inherited = %t", listener, inherited)
	}
}

func TestActivatedListenerRejectsInvalidEnvironment(t *testing.T) {
	processID := os.Getpid()
	for _, test := range []struct {
		name      string
		listenPID string
		listenFDs string
	}{
		{name: "PID", listenPID: "invalid", listenFDs: "1"},
		{name: "descriptor count", listenPID: strconv.Itoa(processID), listenFDs: "invalid"},
		{name: "no descriptors", listenPID: strconv.Itoa(processID), listenFDs: "0"},
		{name: "many descriptors", listenPID: strconv.Itoa(processID), listenFDs: "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, inherited, err := activatedListener(processID, test.listenPID, test.listenFDs, systemdListenFD)
			if err == nil {
				t.Fatalf("listener = %v, inherited = %t", listener, inherited)
			}
		})
	}
}

func TestNoAuthAddressRequiresLiteralLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8317", "127.0.0.2:8317", "[::1]:8317"} {
		if err := validateNoAuthAddress(address); err != nil {
			t.Fatalf("loopback rejected: %v", err)
		}
	}
	for _, address := range []string{":8317", "0.0.0.0:8317", "[::]:8317", "192.168.1.2:8317", "localhost:8317", "invalid"} {
		if err := validateNoAuthAddress(address); err == nil {
			t.Fatalf("nonliteral/nonloopback address accepted: %s", address)
		}
	}
}

func TestNoAuthChecksEffectiveActivatedListener(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "0.0.0.0:0"} {
		t.Run(address, func(t *testing.T) {
			base, err := net.Listen("tcp4", address)
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			file, err := base.(*net.TCPListener).File()
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			listener, inherited, err := activatedListener(os.Getpid(), strconv.Itoa(os.Getpid()), "1", file.Fd())
			if err != nil || !inherited {
				t.Fatalf("socket activation: inherited=%v, err=%v", inherited, err)
			}
			defer listener.Close()
			err = validateNoAuthListener(listener)
			if (err == nil) != (address == "127.0.0.1:0") {
				t.Fatalf("listener %s validation: %v", address, err)
			}
		})
	}
}
