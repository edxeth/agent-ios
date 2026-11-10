package cdp

import (
	"slices"
	"testing"
	"time"
)

// WS2b-1b-b item 3: the page runs the Timeline.enable of a Tracing.start
// but its reply never comes, and the detach cancels the call. The enable
// may have reached the page, so the attachment's owner keeps Timeline and
// the detach's release sends Timeline.disable (core's releaseFailedAcquire
// rule for an enable of unknown outcome).
func TestCDPTimelineEnableWithAnUnknownOutcomeIsDisabledAtDetach(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	tab, page := puppeteerAttach(t, c)
	f.DropReplies("Timeline.enable", 1)
	mark := len(f.Received())
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(ctxFor(t, 20*time.Second), page, "Tracing.start", nil)
		done <- err
	}()
	eventually(t, 5*time.Second, "Timeline.enable received", func() bool { return countSince(f, mark, "Timeline.enable") > 0 })
	detachAndWait(t, c, tab, page)
	if err := <-done; err == nil {
		t.Fatal("Tracing.start with a lost Timeline.enable reply succeeded")
	}
	// A disable after the enable (the start may reset Timeline before it).
	disableAfterEnable := func() bool {
		sent := sinceMark(f, mark)
		i := slices.Index(sent, "Timeline.enable")
		return i >= 0 && slices.Contains(sent[i:], "Timeline.disable")
	}
	end := time.Now().Add(5 * time.Second)
	for !disableAfterEnable() {
		if time.Now().After(end) {
			t.Fatalf("no Timeline.disable after the enable within 5s; the page received %v", sinceMark(f, mark))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := r.pageStateOf(t, "One"); st.Needs("Timeline") {
		t.Fatal("the Timeline domain stayed after the detach")
	}
}
