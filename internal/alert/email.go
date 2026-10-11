package alert

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type Email struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

func (e *Email) Send(ctx context.Context, to string, msg Message) error {
	rcpt, err := mail.ParseAddress(strings.TrimSpace(to))
	if err != nil {
		return permanentError{fmt.Errorf("email: destinatário inválido: %w", err)}
	}
	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return permanentError{fmt.Errorf("email: remetente inválido: %w", err)}
	}

	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	var conn net.Conn
	if e.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: e.Host}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("email: conectar %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("email: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok && e.Port != 465 {
		if err := c.StartTLS(&tls.Config{ServerName: e.Host}); err != nil {
			return fmt.Errorf("email: starttls: %w", err)
		}
	}
	if e.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.Username, e.Password, e.Host)); err != nil {
			return permanentError{fmt.Errorf("email: auth: %w", err)}
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("email: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if _, err := w.Write(buildMessage(from, rcpt, msg)); err != nil {
		return fmt.Errorf("email: escrever corpo: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: fechar DATA: %w", err)
	}
	return c.Quit()
}

func buildMessage(from, to *mail.Address, msg Message) []byte {
	var b strings.Builder
	headers := [][2]string{
		{"From", from.String()},
		{"To", to.String()},
		{"Subject", mime.QEncoding.Encode("utf-8", msg.Subject)},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "8bit"},
	}
	for _, h := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(msg.Text, "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
