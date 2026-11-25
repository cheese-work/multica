package connectedapps

import (
	"errors"
	"testing"
	"time"
)

func TestVerifyTaskTokenExpiryBoundary(t *testing.T) {
	secret := DeriveTokenSecret([]byte("test-service-secret"))
	exp := time.Unix(1_800_000_000, 0)
	token, err := SignTaskToken(secret, "task-1", exp)
	if err != nil {
		t.Fatalf("SignTaskToken: %v", err)
	}

	cases := []struct {
		name    string
		now     time.Time
		wantErr error
	}{
		{"one second before expiry is accepted", exp.Add(-time.Second), nil},
		{"exact expiry is rejected", exp, ErrTokenExpired},
		{"sub-second before expiry is accepted", exp.Add(-time.Millisecond), nil},
		{"one second after expiry is rejected", exp.Add(time.Second), ErrTokenExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taskID, err := VerifyTaskToken(secret, token, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && taskID != "task-1" {
				t.Fatalf("taskID = %q, want task-1", taskID)
			}
			if tc.wantErr != nil && taskID != "" {
				t.Fatalf("taskID = %q on rejected token, want empty", taskID)
			}
		})
	}
}
