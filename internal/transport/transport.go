// Package transport abstracts the byte stream between the hostrun client
// and the hostrunner daemon, so other carriers (e.g. TCP for Windows/macOS
// hosts) can be added without touching the protocol or daemon logic.
package transport

import (
	"context"
	"net"
)

// Transport opens both ends of a client–daemon connection.
type Transport interface {
	// Listen starts accepting connections on the daemon side.
	Listen() (net.Listener, error)
	// Dial connects the client to a listening daemon.
	Dial(ctx context.Context) (net.Conn, error)
}
