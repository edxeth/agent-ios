package direct

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The failure tests make go-ios log every failed handshake.
	SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
