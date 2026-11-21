// Command portfixture runs a TCP listener inside the live conformance guest.
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/sudosylabs/execenv/execenvtest/internal/portfixture"
)

func main() {
	network := "tcp4"
	switch os.Args[1] {
	case "::":
		network = "tcp"
	case "::1":
		network = "tcp6"
	}
	addr, err := net.ResolveTCPAddr(network, net.JoinHostPort(os.Args[1], "0"))
	if err != nil {
		panic(err)
	}
	listener, err := net.ListenTCP(network, addr)
	if err != nil {
		panic(err)
	}
	fmt.Println(listener.Addr().(*net.TCPAddr).Port)
	portfixture.Serve(listener, os.Args[2])
}
