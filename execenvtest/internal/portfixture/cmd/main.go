// Command portfixture runs a TCP listener inside the live conformance guest.
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/sudosylabs/execenv/execenvtest/internal/portfixture"
)

func main() {
	addr, err := net.ResolveTCPAddr("tcp4", os.Args[1]+":0")
	if err != nil {
		panic(err)
	}
	listener, err := net.ListenTCP("tcp4", addr)
	if err != nil {
		panic(err)
	}
	fmt.Println(listener.Addr().(*net.TCPAddr).Port)
	portfixture.Serve(listener, os.Args[2])
}
