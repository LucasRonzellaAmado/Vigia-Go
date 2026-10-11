package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Kind string

const (
	KindHTTP      Kind = "http"
	KindTCP       Kind = "tcp"
	KindPing      Kind = "ping"
	KindSSL       Kind = "ssl"
	KindHeartbeat Kind = "heartbeat"
)

func (k Kind) Valid() bool {
	switch k {
	case KindHTTP, KindTCP, KindPing, KindSSL, KindHeartbeat:
		return true
	}
	return false
}

type Target struct {
	Kind           Kind
	Address        string
	Timeout        time.Duration
	Retries        int
	ExpectedStatus int
	Keyword        string
	SSLExpiryDays  int
	Interval       time.Duration
	LastHeartbeat  time.Time
	CreatedAt      time.Time
}

type Result struct {
	OK         bool
	Latency    time.Duration
	StatusCode int
	Message    string
	CheckedAt  time.Time
}

type PingFunc func(ctx context.Context, host string) (time.Duration, error)

type Checker struct {
	HTTPClient *http.Client
	TLSConfig  *tls.Config
	Dialer     *net.Dialer
	Ping       PingFunc
	Now        func() time.Time
	RetryDelay time.Duration
	MaxBody    int64
}

const userAgent = "Vigia/1.0 (+https://github.com/luquinha/vigia)"

func New() *Checker {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: -1}
	transport := &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            dialer.DialContext,
		DisableKeepAlives:      true,
		TLSHandshakeTimeout:    15 * time.Second,
		ResponseHeaderTimeout:  60 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("redirecionamentos demais (máximo 5)")
			}
			return nil
		},
	}
	return &Checker{
		HTTPClient: client,
		Dialer:     dialer,
		Ping:       ICMPPing,
		Now:        time.Now,
		RetryDelay: 2 * time.Second,
		MaxBody:    1 << 20,
	}
}

func (c *Checker) Check(ctx context.Context, t Target) Result {
	attempts := max(t.Retries, 0) + 1
	if t.Kind == KindHeartbeat {
		attempts = 1
	}

	var res Result
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return res
			case <-time.After(c.RetryDelay * time.Duration(i)):
			}
		}
		res = c.checkOnce(ctx, t)
		if res.OK {
			break
		}
	}
	return res
}

func (c *Checker) checkOnce(ctx context.Context, t Target) Result {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	checkedAt := c.Now().UTC()
	start := time.Now()

	var res Result
	switch t.Kind {
	case KindHTTP:
		res = c.checkHTTP(ctx, t)
	case KindTCP:
		res = c.checkTCP(ctx, t)
	case KindPing:
		res = c.checkPing(ctx, t)
	case KindSSL:
		res = c.checkSSL(ctx, t, checkedAt)
	case KindHeartbeat:
		res = checkHeartbeat(t, checkedAt)
	default:
		res = Result{Message: fmt.Sprintf("tipo de monitor desconhecido: %q", t.Kind)}
	}

	if res.Latency == 0 && t.Kind != KindHeartbeat {
		res.Latency = time.Since(start)
	}
	if res.Message == "" && !res.OK {
		res.Message = "falha"
	}
	if !res.OK && ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Message = fmt.Sprintf("timeout após %s", timeout)
	}
	res.CheckedAt = checkedAt
	return res
}

func (c *Checker) checkHTTP(ctx context.Context, t Target) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Address, nil)
	if err != nil {
		return Result{Message: fmt.Sprintf("URL inválida: %v", err)}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cache-Control", "no-cache")

	start := time.Now()
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return Result{Message: describeErr(err)}
	}
	defer resp.Body.Close()
	latency := time.Since(start)

	res := Result{Latency: latency, StatusCode: resp.StatusCode}

	if !statusMatches(resp.StatusCode, t.ExpectedStatus) {
		res.Message = fmt.Sprintf("status %d (esperado %s)", resp.StatusCode, expectedLabel(t.ExpectedStatus))
		return res
	}

	if t.Keyword != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBody))
		if err != nil {
			res.Message = fmt.Sprintf("erro lendo corpo: %v", describeErr(err))
			return res
		}
		if !strings.Contains(string(body), t.Keyword) {
			res.Message = fmt.Sprintf("palavra-chave %q não encontrada", t.Keyword)
			return res
		}
	}

	res.OK = true
	res.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
	return res
}

func statusMatches(got, expected int) bool {
	if expected == 0 {
		return got >= 200 && got < 400
	}
	return got == expected
}

func expectedLabel(expected int) string {
	if expected == 0 {
		return "2xx/3xx"
	}
	return fmt.Sprint(expected)
}

func (c *Checker) checkTCP(ctx context.Context, t Target) Result {
	conn, err := c.Dialer.DialContext(ctx, "tcp", t.Address)
	if err != nil {
		return Result{Message: describeErr(err)}
	}
	conn.Close()
	return Result{OK: true, Message: "porta aberta"}
}

func (c *Checker) checkPing(ctx context.Context, t Target) Result {
	rtt, err := c.Ping(ctx, t.Address)
	if err != nil {
		return Result{Message: describeErr(err)}
	}
	return Result{OK: true, Latency: rtt, Message: "pong"}
}

func (c *Checker) checkSSL(ctx context.Context, t Target, now time.Time) Result {
	host, addr, err := sslAddress(t.Address)
	if err != nil {
		return Result{Message: err.Error()}
	}

	cfg := &tls.Config{}
	if c.TLSConfig != nil {
		cfg = c.TLSConfig.Clone()
	}
	cfg.ServerName = host

	d := tls.Dialer{NetDialer: c.Dialer, Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{Message: describeErr(err)}
	}
	defer conn.Close()

	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Result{Message: "servidor não apresentou certificado"}
	}
	return evaluateCert(certs[0], now, t.SSLExpiryDays)
}

func evaluateCert(cert *x509.Certificate, now time.Time, minDays int) Result {
	left := cert.NotAfter.Sub(now)
	days := int(left.Hours() / 24)
	expires := cert.NotAfter.UTC().Format("2006-01-02")

	switch {
	case left <= 0:
		return Result{Message: fmt.Sprintf("certificado expirou em %s", expires)}
	case days < minDays:
		return Result{Message: fmt.Sprintf("certificado expira em %d dias (%s)", days, expires)}
	default:
		return Result{OK: true, Message: fmt.Sprintf("certificado válido por %d dias (%s)", days, expires)}
	}
}

func sslAddress(raw string) (host, addr string, err error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("endereço inválido: %w", err)
		}
		raw = u.Host
	}
	if raw == "" {
		return "", "", errors.New("endereço vazio")
	}
	h, p, err := net.SplitHostPort(raw)
	if err != nil {
		return raw, net.JoinHostPort(raw, "443"), nil
	}
	return h, net.JoinHostPort(h, p), nil
}

func checkHeartbeat(t Target, now time.Time) Result {
	ref := t.LastHeartbeat
	if ref.IsZero() {
		ref = t.CreatedAt
	}
	deadline := ref.Add(t.Interval + t.Timeout)
	if now.After(deadline) {
		if t.LastHeartbeat.IsZero() {
			return Result{Message: "nenhum heartbeat recebido ainda"}
		}
		return Result{Message: fmt.Sprintf("sem heartbeat há %s", now.Sub(t.LastHeartbeat).Round(time.Second))}
	}
	if t.LastHeartbeat.IsZero() {
		return Result{OK: true, Message: "aguardando primeiro heartbeat"}
	}
	return Result{OK: true, Message: fmt.Sprintf("último heartbeat há %s", now.Sub(t.LastHeartbeat).Round(time.Second))}
}

func describeErr(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	var opErr *net.OpError

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return fmt.Sprintf("DNS: host %s não encontrado", dnsErr.Name)
		}
		return fmt.Sprintf("DNS: %v", dnsErr.Err)
	case errors.As(err, &unknownAuth):
		return "certificado assinado por autoridade desconhecida"
	case errors.As(err, &hostErr):
		return fmt.Sprintf("certificado não vale para %s", hostErr.Host)
	case errors.As(err, &invalidErr):
		return fmt.Sprintf("certificado inválido: %v", invalidErr)
	case errors.As(err, &certErr):
		return fmt.Sprintf("certificado inválido: %v", certErr.Err)
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return fmt.Sprintf("conexão falhou: %v", opErr.Err)
	}
	return err.Error()
}
