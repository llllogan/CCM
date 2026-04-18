package sshx

import (
	"errors"
	"io"
	"net"
	"testing"
)

func TestIsRetryableConnErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "eof", err: io.EOF, want: true},
		{name: "wrapped eof", err: errors.Join(io.EOF, errors.New("other")), want: true},
		{name: "net closed", err: net.ErrClosed, want: true},
		{name: "broken pipe text", err: errors.New("write tcp: broken pipe"), want: true},
		{name: "connection reset text", err: errors.New("read tcp: connection reset by peer"), want: true},
		{name: "closed network text", err: errors.New("use of closed network connection"), want: true},
		{name: "other", err: errors.New("permission denied"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableConnErr(tt.err); got != tt.want {
				t.Fatalf("isRetryableConnErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
