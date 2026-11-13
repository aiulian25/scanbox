package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"scanbox/internal/settings"
	"scanbox/internal/setup"
)

const unknownHostMessage = "Scan Box doesn't answer to the name %q. If that is how you reach it, add the name to %s in the container's environment."

// knownHostsOnly refuses a request made under a name this box doesn't go by. Otherwise a web
// page elsewhere could point a name of its own at this box and use the page through a
// visitor's browser (DNS rebinding), whose requests then look as if they came from the page itself.
func (s *Server) knownHostsOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host := hostName(request.Host)
		if !s.answersTo(host) {
			http.Error(writer, fmt.Sprintf(unknownHostMessage, host, setup.HostsVariable), http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// hostName takes the port, an IPv6 address's brackets and a closing dot off a Host header.
func hostName(header string) string {
	host, _, err := net.SplitHostPort(header)
	if err != nil {
		host = header
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// answersTo accepts addresses, names without a dot, names that can only be local, and the
// names the container lists. A foreign web page can use none of those for itself.
func (s *Server) answersTo(host string) bool {
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return settings.IsLocalName(host) || slices.Contains(s.options.Hosts, host)
}
