// Command flue listens on a Unix domain socket inside a container
// and proxies every byte to and from a preserved file descriptor (default 3)
// that points at the host's SSH authentication agent socket.
//
// This enables "SSH agent forwarding through podman exec --preserve-fds":
//
//	podman exec \
//  -e SSH_AUTH_SOCK=/tmp/flue.sock \
//	  --preserve-fds 1 \
//  <container> flue
//
// The host passes the forwarded agent socket (FD 3 in the exec'd process) to
// the container; the proxy opens /tmp/flue.sock and shuttles bytes
// between it and FD 3. Inside the container, ssh/ssh-add talk to
// /tmp/flue.sock and transparently reach the real agent on the host.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

func main() {
	sockPath := flag.String("sock", "/tmp/flue.sock",
		"Unix socket path to listen on inside the container")
	fdArg := flag.String("fd", "3",
		"File descriptor number of the preserved host agent socket (default 3)")
	flag.Parse()

	fdNum, err := strconv.Atoi(*fdArg)
	if err != nil || fdNum < 3 {
		log.Fatalf("invalid -fd %q: must be an integer >= 3", *fdArg)
	}

	// Verify the preserved FD is usable before we start.
	var st unix.Stat_t
	if err := unix.Fstat(fdNum, &st); err != nil {
		log.Fatalf("preserved fd %d is not usable: %v", fdNum, err)
	}

	// Remove any stale socket file from a previous run.
	if err := os.Remove(*sockPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("remove stale socket %s: %v", *sockPath, err)
	}

	listener, err := net.Listen("unix", *sockPath)
	if err != nil {
		log.Fatalf("listen %s: %v", *sockPath, err)
	}
	log.Printf("flue: listening on %s, proxying to fd %d", *sockPath, fdNum)

	var wg sync.WaitGroup
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		wg.Add(1)
		go handle(conn, fdNum, &wg)
	}
}

func handle(client net.Conn, fdNum int, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("close client: %v", err)
		}
	}()

	// Each relay direction gets its own buffer so the two goroutines
	// never share a slice they read and write concurrently.
	clientToAgent := make([]byte, 64*1024)
	agentToClient := make([]byte, 64*1024)

	done := make(chan struct{}, 2)
	relayWG := &sync.WaitGroup{}

	// client -> agent: read from the client net.Conn, write raw to the agent fd
	relayWG.Add(1)
	go func() {
		defer relayWG.Done()
		for {
			n, err := client.Read(clientToAgent)
			if n > 0 {
				if _, werr := unix.Write(int(fdNum), clientToAgent[:n]); werr != nil {
					log.Printf("client->agent write: %v", werr)
					break
				}
			}
			if err != nil {
				log.Printf("client->agent read: %v", err)
				break
			}
		}
		done <- struct{}{}
	}()

	// agent -> client: read raw from the agent fd, write to the client net.Conn
	relayWG.Add(1)
	go func() {
		defer relayWG.Done()
		for {
			n, err := unix.Read(int(fdNum), agentToClient)
			if n > 0 {
				if _, werr := client.Write(agentToClient[:n]); werr != nil {
					log.Printf("agent->client write: %v", werr)
					break
				}
			}
			if err != nil {
				// unix.Read returns 0, io.EOF on peer close; that's the
				// normal end of the agent connection.
				if err != unix.Errno(0) {
					log.Printf("agent->client read: %v", err)
				}
				break
			}
		}
		done <- struct{}{}
	}()

	<-done
	relayWG.Wait()
	fmt.Fprintf(os.Stderr, "flue: connection closed\n")
}
