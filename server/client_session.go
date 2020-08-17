package chserver

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"

	chshare "github.com/cloudradar-monitoring/rport/share"
)

func GetSessionID(sshConn ssh.ConnMetadata) string {
	return fmt.Sprintf("%x", sshConn.SessionID())
}

// ClientSession represents active client connection
type ClientSession struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	OS       string    `json:"os"`
	Hostname string    `json:"hostname"`
	IPv4     []string  `json:"ipv4"`
	IPv6     []string  `json:"ipv6"`
	Tags     []string  `json:"tags"`
	Version  string    `json:"version"`
	Address  string    `json:"address"`
	Tunnels  []*Tunnel `json:"tunnels"`

	Connection ssh.Conn        `json:"-"`
	Context    context.Context `json:"-"`
	User       *chshare.User   `json:"-"`
	Logger     *chshare.Logger `json:"-"`

	tunnelIDAutoIncrement int64
	lock                  sync.Mutex
}

func (c *ClientSession) Lock() {
	c.lock.Lock()
}

func (c *ClientSession) Unlock() {
	c.lock.Unlock()
}

func (c *ClientSession) findTunnelByRemote(r *chshare.Remote) *Tunnel {
	for _, curr := range c.Tunnels {
		if curr.Equals(r) {
			return curr
		}
	}
	return nil
}

func (c *ClientSession) StartTunnel(r *chshare.Remote, acl TunnelACL) (*Tunnel, error) {
	t := c.findTunnelByRemote(r)
	if t != nil {
		return t, nil
	}

	tunnelID := strconv.FormatInt(c.generateNewTunnelID(), 10)
	t = NewTunnel(c.Logger, c.Connection, tunnelID, r, acl)
	err := t.Start(c.Context)
	if err != nil {
		return nil, err
	}
	c.Tunnels = append(c.Tunnels, t)
	return t, nil
}

func (c *ClientSession) TerminateTunnel(t *Tunnel) {
	c.Logger.Infof("Terminating tunnel %s...", t.ID)
	t.Terminate()
	c.removeTunnel(t)
}

func (c *ClientSession) FindTunnel(id string) *Tunnel {
	for _, curr := range c.Tunnels {
		if curr.ID == id {
			return curr
		}
	}
	return nil
}

func (c *ClientSession) generateNewTunnelID() int64 {
	return atomic.AddInt64(&c.tunnelIDAutoIncrement, 1)
}

func (c *ClientSession) removeTunnel(t *Tunnel) {
	result := make([]*Tunnel, 0)
	for _, curr := range c.Tunnels {
		if curr.ID != t.ID {
			result = append(result, curr)
		}
	}
	c.Tunnels = result
}

func (c *ClientSession) Banner() string {
	banner := c.ID
	if c.Name != "" {
		banner += " (" + c.Name + ")"
	}
	if len(c.Tags) != 0 {
		for _, t := range c.Tags {
			banner += " #" + t
		}
	}
	return banner
}
