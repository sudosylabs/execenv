package wire

import (
	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

func ListeningPortsTo(ports []execenv.ListeningPort) *execenvv1.ListeningPortsResponse {
	resp := &execenvv1.ListeningPortsResponse{}
	for _, port := range ports {
		resp.Ports = append(resp.Ports, &execenvv1.ListeningPort{Port: int32(port.Port), Address: port.Address, Tag: port.Tag})
	}
	return resp
}

func ListeningPortsFrom(resp *execenvv1.ListeningPortsResponse) []execenv.ListeningPort {
	var ports []execenv.ListeningPort
	for _, port := range resp.GetPorts() {
		ports = append(ports, execenv.ListeningPort{Port: int(port.GetPort()), Address: port.GetAddress(), Tag: port.GetTag()})
	}
	return ports
}
