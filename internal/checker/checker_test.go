package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestChecker() *Checker {
	c := New()
	c.RetryDelay = time.Millisecond
	return c
}

func TestCheckHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "tudo certo por aqui")
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/created":
			w.WriteHeader(http.StatusCreated)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, "lento")
		default:
			http.Error(w, "quebrou", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		target     Target
		wantOK     bool
		wantStatus int
		wantMsg    string
	}{
		{"2xx ok", Target{Kind: KindHTTP, Address: srv.URL + "/ok"}, true, 200, "HTTP 200"},
		{"segue redirect", Target{Kind: KindHTTP, Address: srv.URL + "/redirect"}, true, 200, ""},
		{"500 falha", Target{Kind: KindHTTP, Address: srv.URL + "/boom"}, false, 500, "status 500"},
		{"status esperado exato", Target{Kind: KindHTTP, Address: srv.URL + "/created", ExpectedStatus: 201}, true, 201, ""},
		{"status esperado diferente", Target{Kind: KindHTTP, Address: srv.URL + "/ok", ExpectedStatus: 201}, false, 200, "esperado 201"},
		{"keyword presente", Target{Kind: KindHTTP, Address: srv.URL + "/ok", Keyword: "certo"}, true, 200, ""},
		{"keyword ausente", Target{Kind: KindHTTP, Address: srv.URL + "/ok", Keyword: "errado"}, false, 200, "não encontrada"},
		{"timeout", Target{Kind: KindHTTP, Address: srv.URL + "/slow", Timeout: 50 * time.Millisecond}, false, 0, "timeout"},
		{"url inválida", Target{Kind: KindHTTP, Address: "::nada"}, false, 0, "URL inválida"},
	}

	c := newTestChecker()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := c.Check(context.Background(), tt.target)
			if res.OK != tt.wantOK {
				t.Fatalf("OK = %v, queria %v (msg %q)", res.OK, tt.wantOK, res.Message)
			}
			if res.StatusCode != tt.wantStatus {
				t.Errorf("StatusCode = %d, queria %d", res.StatusCode, tt.wantStatus)
			}
			if tt.wantMsg != "" && !strings.Contains(res.Message, tt.wantMsg) {
				t.Errorf("Message = %q, queria conter %q", res.Message, tt.wantMsg)
			}
			if res.CheckedAt.IsZero() || res.CheckedAt.Location() != time.UTC {
				t.Errorf("CheckedAt deveria estar em UTC: %v", res.CheckedAt)
			}
		})
	}
}

func TestRetriesRecoverFromFlakyFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "soluço", http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	c := newTestChecker()

	res := c.Check(context.Background(), Target{Kind: KindHTTP, Address: srv.URL, Retries: 1})
	if !res.OK {
		t.Fatalf("esperava sucesso após retry, veio %q", res.Message)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("chamadas = %d, queria 2", got)
	}

	calls.Store(0)
	res = c.Check(context.Background(), Target{Kind: KindHTTP, Address: srv.URL, Retries: 0})
	if res.OK {
		t.Fatal("sem retry a primeira falha deveria valer")
	}
}

func TestCheckTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	open := ln.Addr().String()

	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := closedLn.Addr().String()
	closedLn.Close()
	defer ln.Close()

	tests := []struct {
		name   string
		addr   string
		wantOK bool
	}{
		{"porta aberta", open, true},
		{"porta fechada", closed, false},
	}
	c := newTestChecker()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := c.Check(context.Background(), Target{Kind: KindTCP, Address: tt.addr, Timeout: time.Second})
			if res.OK != tt.wantOK {
				t.Fatalf("OK = %v, queria %v (%s)", res.OK, tt.wantOK, res.Message)
			}
		})
	}
}

func TestCheckSSL(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	addr := strings.TrimPrefix(srv.URL, "https://")

	trusted := newTestChecker()
	trusted.TLSConfig = &tls.Config{RootCAs: pool}

	res := trusted.Check(context.Background(), Target{Kind: KindSSL, Address: srv.URL, SSLExpiryDays: 1})
	if !res.OK {
		t.Fatalf("certificado confiável deveria passar: %s", res.Message)
	}

	res = trusted.Check(context.Background(), Target{Kind: KindSSL, Address: addr, SSLExpiryDays: 1_000_000})
	if res.OK || !strings.Contains(res.Message, "expira em") {
		t.Fatalf("deveria acusar expiração próxima: %+v", res)
	}

	untrusted := newTestChecker()
	res = untrusted.Check(context.Background(), Target{Kind: KindSSL, Address: addr, SSLExpiryDays: 1})
	if res.OK || !strings.Contains(res.Message, "certificado") {
		t.Fatalf("certificado autoassinado deveria falhar: %+v", res)
	}
}

func TestEvaluateCert(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		notAfter time.Time
		minDays  int
		wantOK   bool
		wantMsg  string
	}{
		{"folgado", now.AddDate(0, 0, 60), 14, true, "válido por 60 dias"},
		{"perto de vencer", now.AddDate(0, 0, 5), 14, false, "expira em 5 dias"},
		{"vencido", now.AddDate(0, 0, -1), 14, false, "expirou"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := evaluateCert(&x509.Certificate{NotAfter: tt.notAfter}, now, tt.minDays)
			if res.OK != tt.wantOK || !strings.Contains(res.Message, tt.wantMsg) {
				t.Fatalf("res = %+v", res)
			}
		})
	}
}

func TestSSLAddress(t *testing.T) {
	tests := []struct {
		in, host, addr string
	}{
		{"exemplo.com", "exemplo.com", "exemplo.com:443"},
		{"exemplo.com:8443", "exemplo.com", "exemplo.com:8443"},
		{"https://exemplo.com/caminho", "exemplo.com", "exemplo.com:443"},
		{"https://exemplo.com:9443", "exemplo.com", "exemplo.com:9443"},
	}
	for _, tt := range tests {
		host, addr, err := sslAddress(tt.in)
		if err != nil || host != tt.host || addr != tt.addr {
			t.Errorf("sslAddress(%q) = %q, %q, %v", tt.in, host, addr, err)
		}
	}
}

func TestCheckHeartbeat(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		target Target
		wantOK bool
	}{
		{"recebido dentro do prazo", Target{Interval: time.Hour, Timeout: 5 * time.Minute, LastHeartbeat: now.Add(-30 * time.Minute)}, true},
		{"atrasado mas dentro da tolerância", Target{Interval: time.Hour, Timeout: 5 * time.Minute, LastHeartbeat: now.Add(-63 * time.Minute)}, true},
		{"atrasado além da tolerância", Target{Interval: time.Hour, Timeout: 5 * time.Minute, LastHeartbeat: now.Add(-66 * time.Minute)}, false},
		{"nunca bateu, recém-criado", Target{Interval: time.Hour, Timeout: time.Minute, CreatedAt: now.Add(-10 * time.Minute)}, true},
		{"nunca bateu, prazo vencido", Target{Interval: time.Hour, Timeout: time.Minute, CreatedAt: now.Add(-2 * time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := checkHeartbeat(tt.target, now)
			if res.OK != tt.wantOK {
				t.Fatalf("OK = %v, queria %v (%s)", res.OK, tt.wantOK, res.Message)
			}
		})
	}
}

func TestCheckPingUsesInjectedPinger(t *testing.T) {
	c := newTestChecker()
	c.Ping = func(ctx context.Context, host string) (time.Duration, error) {
		if host == "fora.do.ar" {
			return 0, errNoReply
		}
		return 7 * time.Millisecond, nil
	}

	res := c.Check(context.Background(), Target{Kind: KindPing, Address: "no.ar"})
	if !res.OK || res.Latency != 7*time.Millisecond {
		t.Fatalf("res = %+v", res)
	}
	res = c.Check(context.Background(), Target{Kind: KindPing, Address: "fora.do.ar"})
	if res.OK || !strings.Contains(res.Message, "sem resposta") {
		t.Fatalf("res = %+v", res)
	}
}

func TestICMPChecksum(t *testing.T) {
	msg := echoRequest(0x1234, 1, []byte("abc"))
	if icmpChecksum(msg) != 0 {
		t.Fatalf("checksum de mensagem válida deveria zerar, veio %#x", icmpChecksum(msg))
	}
}

func TestICMPPingLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("modo -short")
	}
	conn, _, err := listenICMP()
	if err != nil {
		t.Skipf("ICMP indisponível neste ambiente: %v", err)
	}
	conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rtt, err := ICMPPing(ctx, "127.0.0.1")
	if err != nil {
		t.Fatalf("ping no loopback falhou: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("rtt = %v", rtt)
	}
}
