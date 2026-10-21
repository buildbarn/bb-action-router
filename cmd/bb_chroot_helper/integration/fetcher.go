package integration

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// serveFetcher implements the bb_docker_root_fetcher protocol, serving the
// image roots in roots by image ref.
func serveFetcher(socketPath string, roots map[string]string) (net.Listener, error) {
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleFetcherConn(conn, roots)
		}
	}()
	return listener, nil
}

func handleFetcherConn(conn net.Conn, roots map[string]string) {
	defer conn.Close()
	fmt.Fprintf(conn, "HI\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(line), "ACQUIRE ")
	if root := roots[ref]; ok && root != "" {
		fmt.Fprintf(conn, "OK %s\n", root)
		// Hold the image lease until the helper closes the socket.
		io.Copy(io.Discard, conn)
	} else {
		fmt.Fprintf(conn, "ERROR bad request\n")
	}
}
