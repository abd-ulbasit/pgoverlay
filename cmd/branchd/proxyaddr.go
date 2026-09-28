package main

import (
	"fmt"
	"net"
	"strconv"
)

// advertisedProxy resolves the router address branch API responses advertise
// (proxy_host / proxy_port), which `pgb connect` uses instead of assuming
// "<API host>:6432".
//
// An explicit --advertise-proxy-addr "host:port" wins; the host may be empty
// (":5433") to advertise only the port. Without it, only --pg-addr's port is
// advertised: the listen host (often empty or 0.0.0.0) says nothing about how
// clients reach it, so they keep using the API host. An --pg-addr whose port
// cannot be read advertises nothing.
func advertisedProxy(advertise, pgAddr string) (host string, port int, err error) {
	if advertise != "" {
		h, p, err := net.SplitHostPort(advertise)
		if err != nil {
			return "", 0, fmt.Errorf("--advertise-proxy-addr %q: want host:port (host may be empty): %w", advertise, err)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("--advertise-proxy-addr %q: port must be 1-65535", advertise)
		}
		return h, n, nil
	}
	_, p, err := net.SplitHostPort(pgAddr)
	if err != nil {
		return "", 0, nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, nil
	}
	return "", n, nil
}
