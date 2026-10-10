package main

import (
	"net"
	"sync"
	"testing"
	"time"
)

// TestHandlePerClientAgentConn verifies the core robustness property: each
// client that connects to the flue socket gets a FRESH agent connection, and
// the agent closing its side after one session does not poison later clients.
func TestHandlePerClientAgentConn(t *testing.T) {
	agentSock := t.TempDir() + "/agent.sock"
	agL, err := net.Listen("unix", agentSock)
	if err != nil {
		t.Fatalf("listen agent: %v", err)
	}
	defer agL.Close()

	var agWG sync.WaitGroup
	go func() {
		for {
			conn, err := agL.Accept()
			if err != nil {
				return
			}
			agWG.Add(1)
			go func(conn net.Conn) {
				defer agWG.Done()
				defer conn.Close()
				buf := make([]byte, 1024)
				n, _ := conn.Read(buf)
				if n > 0 {
					conn.Write(append([]byte("echo:"), buf[:n]...))
				}
				conn.Close() // agent closes per session
			}(conn)
		}
	}()

	flueSock := t.TempDir() + "/flue.sock"
	listener, err := net.Listen("unix", flueSock)
	if err != nil {
		t.Fatalf("listen flue: %v", err)
	}
	defer listener.Close()

	var wg, accWG sync.WaitGroup
	agentAddr := &net.UnixAddr{Name: agentSock, Net: "unix"}

	for i := 1; i <= 3; i++ {
		accWG.Add(1)
		go func() {
			defer accWG.Done()
			client, err := listener.Accept()
			if err != nil {
				t.Errorf("flue accept: %v", err)
				return
			}
			wg.Add(1)
			go handle(client, agentAddr, &wg)
		}()

		time.Sleep(50 * time.Millisecond)

		c, err := net.Dial("unix", flueSock)
		if err != nil {
			t.Fatalf("client %d dial: %v", i, err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("list-keys\n")); err != nil {
			t.Fatalf("client %d write: %v", i, err)
		}
		buf := make([]byte, 1024)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("client %d read (expected echo, got: %v)", i, err)
		}
		got := string(buf[:n])
		if got != "echo:list-keys\n" {
			t.Fatalf("client %d: got %q, want %q", i, got, "echo:list-keys\n")
		}
		c.Close()
	}
	wg.Wait()
	accWG.Wait()
	agWG.Wait()
}
