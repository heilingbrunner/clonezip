package webapi

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/heilingbrunner/clonezip/internal/service"
)

// actionsAllowedKey is where actionGuard records its verdict for the rest of
// the request, so /healthz can report it to the dashboard without repeating
// the check.
const actionsAllowedKey = "clonezip.actionsAllowed"

// actionGuard refuses any state-changing request from a client outside the
// configured allowlist, leaving the read-only dashboard open to everyone.
// That makes a service bound to every interface something LAN colleagues can
// watch but not touch, without any credentials to manage. It records its
// verdict for every request, GET included, so a handler exposing more than
// the dashboard needs a remote viewer to see - getSettings, notably - can
// refuse on its own even though the method itself would otherwise pass.
//
// It also refuses cross-site state changes from allowed clients. A browser
// attaches no credentials here to steal, but "the request came from loopback"
// is exactly what a page on some unrelated website can arrange by having the
// operator's own browser POST to http://localhost:8091 - ordinary CSRF, which
// this closes off. What it does not close off is DNS rebinding, where the
// attacker's own hostname resolves to 127.0.0.1 and Origin therefore matches
// Host legitimately; defeating that needs a Host allowlist, which would break
// access through a reverse proxy or any hostname but the expected one.
func actionGuard(allow []netip.Prefix) gin.HandlerFunc {
	return func(c *gin.Context) {
		allowed := clientMayAct(c.Request, allow)
		c.Set(actionsAllowedKey, allowed)

		if isReadMethod(c.Request.Method) {
			c.Next()
			return
		}
		if !allowed {
			c.AbortWithStatusJSON(http.StatusForbidden, errorDTO{
				Error: "read-only: actions are available on the server host only",
			})
			return
		}
		if !isSameSite(c.Request) {
			c.AbortWithStatusJSON(http.StatusForbidden, errorDTO{
				Error: "cross-site request refused",
			})
			return
		}
		c.Next()
	}
}

// isReadMethod reports whether a request only reads. These stay open to every
// client: they are what the monitoring dashboard is made of.
func isReadMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// clientMayAct matches the peer's address against the allowlist.
//
// The address comes from RemoteAddr - the far end of the TCP connection -
// and never from X-Forwarded-For or any other header, which a remote client
// is free to set to 127.0.0.1. NewServer disables gin's own forwarded-header
// handling for the same reason.
func clientMayAct(r *http.Request, allow []netip.Prefix) bool {
	addr, ok := peerAddr(r)
	if !ok {
		return false
	}
	return service.ActionsAllowedFrom(allow, addr)
}

// peerAddr parses r.RemoteAddr, which is "host:port" with the host bracketed
// when it is IPv6, and may carry a zone ("[fe80::1%eth0]:54321").
func peerAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// httptest and unix sockets can leave RemoteAddr portless.
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

// isSameSite reports whether a state-changing request came from the dashboard
// itself rather than from another site in the same browser.
//
// Both signals are absent for non-browser clients (curl, a deployment script),
// which are therefore allowed through: they are not the ones an attacker gets
// to aim at localhost.
func isSameSite(r *http.Request) bool {
	// Sec-Fetch-Site is set by the browser and cannot be forged by script.
	// "none" means the user themselves initiated it (a typed URL, a
	// bookmark), which no cross-site page can produce.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "":
	case "same-origin", "none":
	default:
		return false
	}

	// Origin accompanies every cross-origin fetch, and same-origin ones for
	// the methods that reach here, so a mismatch is decisive.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

// actionsAllowed reports the verdict actionGuard recorded for this request.
func actionsAllowed(c *gin.Context) bool {
	allowed, ok := c.Get(actionsAllowedKey)
	if !ok {
		return false
	}
	b, _ := allowed.(bool)
	return b
}
