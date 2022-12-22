package ports

import (
	"fmt"
	"sync"

	mapset "github.com/deckarep/golang-set"
	"github.com/shirou/gopsutil/net"

	"github.com/cloudradar-monitoring/rport/share/models"
)

type PortDistributor struct {
	allowedPorts mapset.Set

	portsPools map[string]mapset.Set

	mu sync.RWMutex
}

func NewPortDistributor(allowedPorts mapset.Set) *PortDistributor {
	return &PortDistributor{
		allowedPorts: allowedPorts,
		portsPools:   make(map[string]mapset.Set),
	}
}

// NewPortDistributorForTests is used only for unit-testing.
func NewPortDistributorForTests(allowedPorts, tcpPortsPool, udpPortsPool mapset.Set) *PortDistributor {
	return &PortDistributor{
		allowedPorts: allowedPorts,
		portsPools: map[string]mapset.Set{
			models.ProtocolTCP: tcpPortsPool,
			models.ProtocolUDP: udpPortsPool,
		},
	}
}

func (d *PortDistributor) GetPortsPool(p string) mapset.Set {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.portsPools[p]
}

func (d *PortDistributor) SetPortsPool(p string, m mapset.Set) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.portsPools[p] = m
}

func (d *PortDistributor) GetRandomPort(protocol string) (int, error) {
	subProtocols := []string{protocol}
	if protocol == models.ProtocolTCPUDP {
		subProtocols = []string{models.ProtocolTCP, models.ProtocolUDP}
	}
	for _, p := range subProtocols {
		if d.GetPortsPool(p) == nil {
			err := d.refresh(p)
			if err != nil {
				return 0, err
			}
		}
	}

	port := d.getPool(protocol).Pop()
	if port == nil {
		return 0, fmt.Errorf("no ports available")
	}

	// Make sure port is removed from all pools for tcp+udp protocol
	for _, p := range subProtocols {
		d.GetPortsPool(p).Remove(port)
	}

	return port.(int), nil
}

func (d *PortDistributor) IsPortAllowed(port int) bool {
	return d.allowedPorts.Contains(port)
}

func (d *PortDistributor) IsPortBusy(protocol string, port int) bool {
	return !d.getPool(protocol).Contains(port)
}

func (d *PortDistributor) getPool(protocol string) mapset.Set {
	pool := d.GetPortsPool(protocol)
	if protocol == models.ProtocolTCPUDP {
		pool = d.GetPortsPool(models.ProtocolTCP).Intersect(d.GetPortsPool(models.ProtocolUDP))
	}
	return pool
}

func (d *PortDistributor) Refresh() error {
	err := d.refresh(models.ProtocolTCP)
	if err != nil {
		return err
	}
	err = d.refresh(models.ProtocolUDP)
	if err != nil {
		return err
	}
	return nil
}

func (d *PortDistributor) refresh(protocol string) error {
	busyPorts, err := ListBusyPorts(protocol)
	if err != nil {
		return err
	}
	d.SetPortsPool(protocol, d.allowedPorts.Difference(busyPorts))
	return nil
}

func ListBusyPorts(protocol string) (mapset.Set, error) {
	result := mapset.NewSet()
	connections, err := net.Connections(protocol)
	if err != nil {
		return nil, err
	}

	for _, c := range connections {
		isActive := c.Status == "LISTEN" || c.Status == "NONE" || c.Status == ""
		if isActive && c.Laddr.Port != 0 {
			result.Add(int(c.Laddr.Port))
		}
	}

	return result, nil
}
