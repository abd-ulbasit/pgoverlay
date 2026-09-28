package api

// proxyEndpoint is the wire-protocol router address advertised to clients in
// branch responses (proxy_host / proxy_port).
type proxyEndpoint struct {
	host string
	port int
}

// SetProxyEndpoint sets the router address advertised in branch responses, so
// `pgb connect` and other clients stop guessing "<API host>:6432". host may be
// "" (clients fall back to the API host); port 0 advertises no port. Call it
// before serving.
func (s *Server) SetProxyEndpoint(host string, port int) {
	s.proxy = proxyEndpoint{host: host, port: port}
}
