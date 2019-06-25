package net

import (
	"github.com/valyala/fasthttp"
	"net/http"
)

var MaxWorkers uint

// Custom headers
type MainTransport struct{}

// Important:
// - Set header "Accept-Language: en-US" or else parser might break
// - Set header "User-Agent: yt-mango/0.1"
func (t MainTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Add("Accept-Language", "en-US")
	r.Header.Add("User-Agent", "yt-mango/0.1")
	return http.DefaultTransport.RoundTrip(r)
}

var Client = fasthttp.Client{
	Name: "yt-mango/0.1",
	DisableHeaderNamesNormalizing: true,
	MaxConnsPerHost: 10000,
}