package webapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/heilingbrunner/clonezip/internal/service"
)

// guardedEngine mirrors what NewServer builds - the same trusted-proxy
// settings and the same guard - around a pair of do-nothing routes, so these
// tests exercise the access decision alone.
func guardedEngine(t *testing.T, allowActionsFrom ...string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.ForwardedByClientIP = false
	if err := r.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies(nil) = %v", err)
	}
	allow := service.Config{AllowActionsFrom: allowActionsFrom}.ActionAllowList()
	r.Use(actionGuard(allow))

	ok := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	r.GET("/api/groups", ok)
	r.POST("/api/groups", ok)
	r.PUT("/api/settings", ok)
	r.DELETE("/api/groups/x", ok)
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"actionsAllowed": actionsAllowed(c)})
	})

	return r
}

// request issues one request through the guard. remoteAddr is what a real
// listener would put in http.Request.RemoteAddr.
func request(r *gin.Engine, method, path, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestGuardServesReadsToEveryClient(t *testing.T) {
	r := guardedEngine(t)

	for _, remote := range []string{"127.0.0.1:5000", "[::1]:5000", "192.168.1.20:5000", "203.0.113.9:5000"} {
		if got := request(r, http.MethodGet, "/api/groups", remote, nil).Code; got != http.StatusOK {
			t.Errorf("GET /api/groups from %s = %d, want %d", remote, got, http.StatusOK)
		}
	}
}

func TestGuardRefusesRemoteWrites(t *testing.T) {
	r := guardedEngine(t)

	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/groups"},
		{http.MethodPut, "/api/settings"},
		{http.MethodDelete, "/api/groups/x"},
	}
	for _, tc := range cases {
		w := request(r, tc.method, tc.path, "192.168.1.20:5000", nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s from a remote client = %d, want %d", tc.method, tc.path, w.Code, http.StatusForbidden)
		}
	}
}

func TestGuardAllowsLoopbackWrites(t *testing.T) {
	r := guardedEngine(t)

	// The IPv4-mapped form is what a dual-stack listener reports for an IPv4
	// peer on Linux; ::1 is what "localhost" resolves to first on both Linux
	// and Windows.
	for _, remote := range []string{"127.0.0.1:5000", "127.0.0.2:5000", "[::1]:5000", "[::ffff:127.0.0.1]:5000"} {
		if got := request(r, http.MethodPost, "/api/groups", remote, nil).Code; got != http.StatusOK {
			t.Errorf("POST /api/groups from %s = %d, want %d", remote, got, http.StatusOK)
		}
	}
}

// A remote client must not be able to talk its way into the allowlist with a
// header. This is the failure gin's default trusted-proxy setting would hand
// out for free.
func TestGuardIgnoresForwardedHeaders(t *testing.T) {
	r := guardedEngine(t)

	spoofs := []map[string]string{
		{"X-Forwarded-For": "127.0.0.1"},
		{"X-Real-IP": "127.0.0.1"},
		{"X-Forwarded-For": "127.0.0.1, 192.168.1.20"},
	}
	for _, headers := range spoofs {
		w := request(r, http.MethodPost, "/api/groups", "192.168.1.20:5000", headers)
		if w.Code != http.StatusForbidden {
			t.Errorf("POST /api/groups with %v = %d, want %d", headers, w.Code, http.StatusForbidden)
		}
	}
}

func TestGuardRefusesCrossSiteWrites(t *testing.T) {
	r := guardedEngine(t)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site fetch metadata", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"foreign origin", map[string]string{"Origin": "http://evil.example"}},
		{"origin on another port", map[string]string{"Origin": "http://127.0.0.1:9999"}},
	}
	for _, tc := range cases {
		w := request(r, http.MethodPost, "/api/groups", "127.0.0.1:5000", tc.headers)
		if w.Code != http.StatusForbidden {
			t.Errorf("POST /api/groups from loopback with %s = %d, want %d", tc.name, w.Code, http.StatusForbidden)
		}
	}
}

func TestGuardAllowsSameOriginAndNonBrowserWrites(t *testing.T) {
	r := guardedEngine(t)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		// httptest.NewRequest sets Host to "example.com".
		{"dashboard fetch", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://example.com"}},
		{"user-initiated", map[string]string{"Sec-Fetch-Site": "none"}},
		{"curl or a script", nil},
	}
	for _, tc := range cases {
		w := request(r, http.MethodPost, "/api/groups", "127.0.0.1:5000", tc.headers)
		if w.Code != http.StatusOK {
			t.Errorf("POST /api/groups from loopback as %s = %d, want %d", tc.name, w.Code, http.StatusOK)
		}
	}
}

func TestGuardHonoursConfiguredAllowList(t *testing.T) {
	r := guardedEngine(t, "192.168.1.0/24")

	if got := request(r, http.MethodPost, "/api/groups", "192.168.1.20:5000", nil).Code; got != http.StatusOK {
		t.Errorf("POST /api/groups from an allowed LAN client = %d, want %d", got, http.StatusOK)
	}
	// An explicit list replaces the loopback default rather than extending it.
	if got := request(r, http.MethodPost, "/api/groups", "127.0.0.1:5000", nil).Code; got != http.StatusForbidden {
		t.Errorf("POST /api/groups from loopback = %d, want %d", got, http.StatusForbidden)
	}
}

func TestHealthReportsActionsAllowed(t *testing.T) {
	r := guardedEngine(t)

	if body := request(r, http.MethodGet, "/healthz", "127.0.0.1:5000", nil).Body.String(); body != `{"actionsAllowed":true}` {
		t.Errorf("/healthz from loopback = %s, want actionsAllowed true", body)
	}
	if body := request(r, http.MethodGet, "/healthz", "192.168.1.20:5000", nil).Body.String(); body != `{"actionsAllowed":false}` {
		t.Errorf("/healthz from a remote client = %s, want actionsAllowed false", body)
	}
}

func TestGuardRefusesUnparseableRemoteAddr(t *testing.T) {
	r := guardedEngine(t)

	if got := request(r, http.MethodPost, "/api/groups", "not-an-address", nil).Code; got != http.StatusForbidden {
		t.Errorf("POST /api/groups from an unparseable peer = %d, want %d", got, http.StatusForbidden)
	}
}
