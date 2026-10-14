// Package proxypool 管理 SOCKS5 代理池：轮询/随机/粘号策略、健康检查、
// 失败冷却。自带最小 SOCKS5 dialer（支持无认证与用户名密码），零第三方依赖。
package proxypool

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Proxy 单个代理节点。
type Proxy struct {
	URL         string
	Scheme      string // socks5 / socks5h / http
	Host        string
	Username    string
	Password    string
	Healthy     bool
	FailCount   int64
	SuccessCount int64
	CooldownUntil time.Time
	LastCheck   time.Time
	LastError   string
}

// Pool 代理池。
type Pool struct {
	mu           sync.Mutex
	proxies      []*Proxy
	policy       string // round-robin | random | sticky-key
	healthEvery  time.Duration
	failCooldown time.Duration
	rr           int
	rng          *rand.Rand
	stopCh       chan struct{}
}

// New 创建代理池。
func New(policy string, healthEvery, failCooldown time.Duration) *Pool {
	if policy == "" {
		policy = "round-robin"
	}
	p := &Pool{
		policy:       policy,
		healthEvery:  healthEvery,
		failCooldown: failCooldown,
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
		stopCh:       make(chan struct{}),
	}
	go p.healthLoop()
	return p
}

// SetPolicy 更新策略。
func (p *Pool) SetPolicy(policy string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = policy
}

// Sync 整池替换（按行粘贴的地址列表）；空列表=直连。
func (p *Pool) Sync(urls []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*Proxy
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		pr, err := parseProxyURL(u)
		if err != nil {
			continue
		}
		out = append(out, pr)
	}
	p.proxies = out
	p.rr = 0
}

// Parse 解析单个代理 URL（供账号专用代理使用）。
func Parse(raw string) (*Proxy, error) { return parseProxyURL(raw) }

// parseProxyURL 解析 socks5://[user:pass@]host:port。
func parseProxyURL(raw string) (*Proxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "socks5" && scheme != "socks5h" && scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("不支持的代理协议: %s", scheme)
	}
	pr := &Proxy{URL: raw, Scheme: scheme, Host: u.Host, Healthy: true}
	if u.User != nil {
		pr.Username = u.User.Username()
		pr.Password, _ = u.User.Password()
	}
	return pr, nil
}

// healthyLocked 健康且不在冷却中。
func (p *Pool) healthyLocked(now time.Time) []*Proxy {
	var out []*Proxy
	for _, pr := range p.proxies {
		if !pr.Healthy || now.Before(pr.CooldownUntil) {
			continue
		}
		out = append(out, pr)
	}
	return out
}

// Pick 按策略选代理；返回 nil 表示直连。
// stickyKey 非空时走 sticky-key 策略（同一 key 固定代理）。
func (p *Pool) Pick(stickyKey string) *Proxy {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cands := p.healthyLocked(now)
	if len(cands) == 0 {
		return nil
	}
	switch p.policy {
	case "random":
		return cands[p.rng.Intn(len(cands))]
	case "sticky-key":
		if stickyKey != "" {
			h := 0
			for _, c := range []byte(stickyKey) {
				h = h*31 + int(c)
			}
			if h < 0 {
				h = -h
			}
			return cands[h%len(cands)]
		}
		fallthrough
	default: // round-robin
		pr := cands[p.rr%len(cands)]
		p.rr++
		return pr
	}
}

// ReportSuccess / ReportFailure 记录代理使用结果。
func (p *Pool) ReportSuccess(pr *Proxy) {
	if pr == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pr.SuccessCount++
	pr.FailCount = 0
}

func (p *Pool) ReportFailure(pr *Proxy, err error) {
	if pr == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pr.FailCount++
	pr.LastError = err.Error()
	pr.CooldownUntil = time.Now().Add(p.failCooldown)
}

// Snapshot 脱敏快照。
func (p *Pool) Snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.proxies))
	for _, pr := range p.proxies {
		host := pr.Host
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		masked := pr.Scheme + "://***@" + host
		if pr.Username == "" {
			masked = pr.Scheme + "://" + host
		}
		out = append(out, map[string]any{
			"url":            masked,
			"healthy":        pr.Healthy,
			"success":        pr.SuccessCount,
			"fail":           pr.FailCount,
			"last_error":     pr.LastError,
			"cooldown_until": pr.CooldownUntil.Format(time.RFC3339),
		})
	}
	return out
}

// Count 节点数。
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.proxies)
}

// Close 停止健康检查。
func (p *Pool) Close() { close(p.stopCh) }

func (p *Pool) healthLoop() {
	iv := p.healthEvery
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			p.checkAll()
		}
	}
}

func (p *Pool) checkAll() {
	p.mu.Lock()
	proxies := append([]*Proxy{}, p.proxies...)
	p.mu.Unlock()
	for _, pr := range proxies {
		ok := probeProxy(pr)
		p.mu.Lock()
		pr.LastCheck = time.Now()
		pr.Healthy = ok
		if !ok {
			pr.LastError = "健康检查失败"
		}
		p.mu.Unlock()
	}
}

// probeProxy 用代理连一个探测目标（TCP 建连即可）。
func probeProxy(pr *Proxy) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	d := &net.Dialer{}
	var conn net.Conn
	var err error
	if pr.Scheme == "http" || pr.Scheme == "https" {
		conn, err = d.DialContext(ctx, "tcp", pr.Host)
	} else {
		conn, err = socks5Dial(ctx, d, pr, "www.qq.com:443")
	}
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// DialContext 经代理（可为 nil=直连）建立到 addr 的 TCP 连接。
func DialContext(ctx context.Context, d *net.Dialer, pr *Proxy, addr string) (net.Conn, error) {
	if pr == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	if pr.Scheme == "http" || pr.Scheme == "https" {
		return httpProxyDial(ctx, d, pr, addr)
	}
	return socks5Dial(ctx, d, pr, addr)
}

// Transport 返回配置好代理的 http.Transport（pr=nil 则直连）。
func Transport(pr *Proxy) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return DialContext(ctx, &net.Dialer{Timeout: 15 * time.Second}, pr, addr)
		},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
}

// ---- SOCKS5 ----

func socks5Dial(ctx context.Context, d *net.Dialer, pr *Proxy, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp", pr.Host)
	if err != nil {
		return nil, err
	}
	if err := socks5Handshake(conn, pr, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func socks5Handshake(conn net.Conn, pr *Proxy, addr string) error {
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	defer conn.SetDeadline(time.Time{})
	br := bufio.NewReader(conn)
	// 1. 认证方法协商
	var hello []byte
	if pr.Username != "" {
		hello = []byte{0x05, 0x02, 0x00, 0x02} // 无认证 / 用户名密码
	} else {
		hello = []byte{0x05, 0x01, 0x00}
	}
	if _, err := conn.Write(hello); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := io_ReadFull(br, resp); err != nil {
		return fmt.Errorf("socks5 握手失败: %w", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5 版本异常: %d", resp[0])
	}
	switch resp[1] {
	case 0x00: // 无需认证
	case 0x02: // 用户名密码
		u, pw := []byte(pr.Username), []byte(pr.Password)
		if len(u) > 255 || len(pw) > 255 {
			return fmt.Errorf("socks5 用户名/密码过长")
		}
		req := []byte{0x01, byte(len(u))}
		req = append(req, u...)
		req = append(req, byte(len(pw)))
		req = append(req, pw...)
		if _, err := conn.Write(req); err != nil {
			return err
		}
		ar := make([]byte, 2)
		if _, err := io_ReadFull(br, ar); err != nil {
			return fmt.Errorf("socks5 认证失败: %w", err)
		}
		if ar[1] != 0x00 {
			return fmt.Errorf("socks5 用户名密码被拒绝")
		}
	default:
		return fmt.Errorf("socks5 不支持的认证方法: %d", resp[1])
	}
	// 2. CONNECT
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5 主机名过长")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	hdr := make([]byte, 4)
	if _, err := io_ReadFull(br, hdr); err != nil {
		return fmt.Errorf("socks5 connect 应答失败: %w", err)
	}
	if hdr[1] != 0x00 {
		return fmt.Errorf("socks5 connect 被拒绝: code=%d", hdr[1])
	}
	// 跳过 BND.ADDR
	var skip int
	switch hdr[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		ln := make([]byte, 1)
		if _, err := io_ReadFull(br, ln); err != nil {
			return err
		}
		skip = int(ln[0])
	default:
		return fmt.Errorf("socks5 地址类型异常: %d", hdr[3])
	}
	if _, err := io_ReadFull(br, make([]byte, skip+2)); err != nil {
		return err
	}
	return nil
}

// io_ReadFull 避免引入 io 包名冲突的小包装（实际直接用 io）。
func io_ReadFull(r *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// ---- HTTP CONNECT 代理 ----

func httpProxyDial(ctx context.Context, d *net.Dialer, pr *Proxy, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp", pr.Host)
	if err != nil {
		return nil, err
	}
	req := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if pr.Username != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(pr.Username+":"+pr.Password)))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	// CONNECT 应答之后对端可能立刻开始发送数据（如 TLS 握手），
	// bufio.Reader 预读进缓冲的字节必须接回去，否则会被丢弃。
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		conn.Close()
		return nil, fmt.Errorf("http 代理 CONNECT 失败: %s", resp.Status)
	}
	if br.Buffered() > 0 {
		prefix, _ := br.Peek(br.Buffered())
		return &prefixConn{Conn: conn, prefix: prefix}, nil
	}
	return conn, nil
}

// prefixConn 在连接开头拼接一段已预读的字节。
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}
