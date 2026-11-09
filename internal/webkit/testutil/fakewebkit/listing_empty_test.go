package fakewebkit

import (
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// EmptyReplyWhileConnected models the iwdp of the second device check:
// the device list and the page list close each request without a reply
// while a WebSocket to a page is open, and answer again after it closed.
func TestListingEmptyReplyWhileConnected(t *testing.T) {
	s := New(Options{})
	t.Cleanup(s.Close)
	l := NewListing(false)
	t.Cleanup(l.Close)
	d := l.AddDevice("udid-1", "iPhone X", "16.7.10")
	d.AddPage("Fixture", "https://fixture.example/", s.URL)
	l.EmptyReplyWhileConnected(s)
	answers := func(addr string) bool {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/json")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	if !answers(l.Addr()) || !answers(d.Addr()) {
		t.Fatal("the listing does not answer without a connection")
	}
	c, _, err := websocket.DefaultDialer.Dial(s.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); s.OpenConnections() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the server did not count the connection")
		}
	}
	if answers(l.Addr()) || answers(d.Addr()) {
		t.Fatal("the listing answered while a page connection is open")
	}
	_ = c.Close()
	for end := time.Now().Add(5 * time.Second); !answers(l.Addr()) || !answers(d.Addr()); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the listing does not answer again after the connection closed")
		}
	}
}
