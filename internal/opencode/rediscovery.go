package opencode

import (
	"context"
	"time"
)

// connection returns the address and credentials as one coherent snapshot.
func (c *Client) connection() (string, string) {
	c.endpointMu.RLock()
	defer c.endpointMu.RUnlock()
	return c.baseURL, c.password
}

// rediscover is only enabled for auto mode. Never replay a failed request:
// especially for mutations, the old server may already have accepted it.
func (c *Client) rediscover(ctx context.Context) bool {
	c.endpointMu.RLock()
	options := c.discovery
	oldBase, oldPassword := c.baseURL, c.password
	c.endpointMu.RUnlock()
	if options == nil || ctx.Err() != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := Discover(probeCtx, *options)
	if err != nil || info == nil || (info.BaseURL == oldBase && info.Password == oldPassword) {
		return false
	}
	// Lock order agrees with protocol detection. Don't overwrite a newer result.
	c.protocolMu.Lock()
	defer c.protocolMu.Unlock()
	c.endpointMu.Lock()
	defer c.endpointMu.Unlock()
	if c.baseURL != oldBase || c.password != oldPassword {
		return false
	}
	c.baseURL, c.password = info.BaseURL, info.Password
	c.protocol = 0
	return true
}
