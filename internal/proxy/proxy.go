package proxy

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"github.com/gin-gonic/gin"
)

type Proxy struct {
	rp         *httputil.ReverseProxy
	target     *url.URL
	instanceID string
}

func New(backendURL string) *Proxy {
	target, err := url.Parse(backendURL)
	if err != nil || target.Host == "" {
		panic("invalid BACKEND_URL: " + backendURL)
	}
	rp := httputil.NewSingleHostReverseProxy(target)

	// One shared Transport with keep-alive so we reuse TCP connections to the
	// backend instead of dialing per request. (Durations must be time.Duration
	// values; a bare integer is interpreted as nanoseconds.)
	rp.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   200,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
	}

	id := os.Getenv("INSTANCE_ID")
	if id == "" {
		id = "gateway"
	}
	return &Proxy{rp: rp, target: target, instanceID: id}
}

func (p *Proxy) Handle(c *gin.Context) {
	// Strip gateway credentials before forwarding
	c.Request.Header.Del("Authorization")
	c.Request.Header.Del("X-API-Key")

	// Add client identity headers so the backend knows who was authenticated
	c.Request.Header.Set("X-Client-Id", c.GetString("client_id"))
	c.Request.Header.Set("X-Client-Tier", c.GetString("client_tier"))

	// Identify which replica handled this request (useful for load-balancer tests)
	c.Writer.Header().Set("X-Gateway-Instance", p.instanceID)

	// Strip the /v1/proxy prefix before forwarding
	c.Request.URL.Path = c.Param("path")
	c.Request.URL.RawPath = ""

	// Virtual-hosted upstreams (e.g. https://httpbin.org) route on the Host
	// header, so present the upstream's host rather than the gateway's.
	c.Request.Host = p.target.Host

	p.rp.ServeHTTP(c.Writer, c.Request)
}
