// Package otp issues and verifies one-time verification codes (e.g. for SMS or
// email login). Delivery is abstracted behind Sender so the mock used today can
// be swapped for a real SMS/email provider without touching the call sites.
package otp

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Channel is how a code is delivered.
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
)

// ChannelFor picks the delivery channel from the target's shape: anything with
// an "@" is treated as an email, everything else as a phone number.
func ChannelFor(target string) Channel {
	if strings.Contains(target, "@") {
		return ChannelEmail
	}
	return ChannelSMS
}

// Sender delivers a code to a target. Implementations are the only place that
// know about a concrete SMS/email provider.
type Sender interface {
	Send(ctx context.Context, channel Channel, target, code string) error
}

// MockSender stands in for a real provider: it logs the code and keeps the most
// recent one per target in memory. This makes local development and tests easy
// ("what code did we send to X?") without any external service. Swap it for a
// real Sender in main.go when the SMS/email integration lands.
type MockSender struct {
	mu   sync.Mutex
	last map[string]string
}

func NewMockSender() *MockSender {
	return &MockSender{last: make(map[string]string)}
}

func (m *MockSender) Send(_ context.Context, channel Channel, target, code string) error {
	m.mu.Lock()
	m.last[target] = code
	m.mu.Unlock()

	slog.Info("otp_code_sent",
		"mock", true,
		"channel", channel,
		"target", target,
		"code", code, // safe to log: mock provider, dev/test only
	)
	return nil
}

// LastCode returns the most recent code "sent" to target, or "" if none. Useful
// for tests and local manual flows.
func (m *MockSender) LastCode(target string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last[target]
}
