package ws

import (
	"testing"
	"time"

	"gohome/shared/security"
)

func TestValidateTimeKeyTracksFailures(t *testing.T) {
	session := &Session{failures: 2}
	now := time.Now()
	if reply := validateTimeKey(session, "ping-1", "secret", security.GenerateTimeKey("secret", now), now.Unix()); reply != nil {
		t.Fatalf("valid time key rejected: %+v", reply.Error)
	}
	if session.failures != 0 {
		t.Fatalf("valid time key did not reset failures: %d", session.failures)
	}
	if reply := validateTimeKey(session, "ping-2", "secret", "bad", now.Unix()); reply == nil || reply.Error == nil || reply.Error.Code != "time_key_invalid" {
		t.Fatalf("invalid time key reply got %+v", reply)
	}
	if session.failures != 1 {
		t.Fatalf("invalid time key failure count got %d want 1", session.failures)
	}
}
