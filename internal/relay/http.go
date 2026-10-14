package relay

import (
	"net/http"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/proxypool"
)

// newHTTPClient 创建绑定代理的上游 HTTP 客户端。
func newHTTPClient(timeout time.Duration, pr *proxypool.Proxy) *http.Client {
	tr := proxypool.Transport(pr)
	tr.DisableCompression = false
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
	}
}
