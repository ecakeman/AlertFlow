package notify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

type emailConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	To       string `json:"to"`
}

type emailSender struct{}

func init() { register("email", emailSender{}) }

// Send delivers via SMTP with STARTTLS/plain auth (v1 targets the common port
// 587 pattern; implicit-TLS on port 465 is not supported). The dial, greeting,
// and later protocol steps all stop when ctx is cancelled or its deadline passes.
func (emailSender) Send(ctx context.Context, raw json.RawMessage, msg Message) error {
	var cfg emailConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg.Host == "" || cfg.From == "" || cfg.To == "" {
		return errors.New("email: host, from and to are required")
	}
	port := cfg.Port
	if port == 0 {
		port = 587
	}
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Hello("localhost"); err != nil {
		return err
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	for _, rcpt := range splitRecipients(cfg.To) {
		if err := client.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildMessage(cfg, msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// buildMessage renders a minimal RFC 822 text/plain email (pure; unit-tested).
func buildMessage(cfg emailConfig, msg Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", cfg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject())
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(msg.Body())
	return []byte(b.String())
}

// splitRecipients splits a comma-separated recipient list, trimming blanks.
func splitRecipients(to string) []string {
	parts := strings.Split(to, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
