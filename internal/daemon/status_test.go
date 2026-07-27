package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrometest"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// listBrowser is a Browser whose List answers however the test needs — the one
// call __status makes, and therefore the only evidence it has that the CDP
// connection is still alive.
type listBrowser struct {
	chrometest.StubBrowser
	tabs []target.Info
	err  error
}

func (b listBrowser) List(context.Context) ([]target.Info, error) { return b.tabs, b.err }

// TestStatusReportsAFailedListAsNotConnected is the daemon half of the doctor
// defect: __status used to hardcode "connected": true and treat a List failure
// as "omit targets". A daemon whose Chrome has quit holds a dead chromedp
// connection but keeps its listener for the whole idle window, so `running` is
// no evidence at all — the List it already performs is.
func TestStatusReportsAFailedListAsNotConnected(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name          string
		b             listBrowser
		wantConnected bool
		wantCount     int
	}{
		{"list fails", listBrowser{err: errors.New("websocket: close 1006")}, false, 0},
		{"list works", listBrowser{tabs: []target.Info{{ID: "a"}, {ID: "b"}}}, true, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := &server{b: c.b}
			res, err := s.dispatch(t.Context(), "__status", nil)
			if err != nil {
				t.Fatalf("__status: %v", err)
			}
			info, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("__status returned %T, want a map", res)
			}
			if info["connected"] != c.wantConnected {
				t.Errorf("connected = %v, want %v — a status that always says true verifies nothing", info["connected"], c.wantConnected)
			}
			if got, _ := info["target_count"].(int); got != c.wantCount {
				t.Errorf("target_count = %v, want %d", info["target_count"], c.wantCount)
			}
		})
	}
}

// TestStatusPublishesNoTabTitlesOrURLs: the daemon's status payload is what
// `doctor --json` echoes, and SKILL.md makes doctor step 1 of every agent
// session. Open-tab URLs (OAuth callbacks, reset tokens, internal hostnames)
// must not ride along.
func TestStatusPublishesNoTabTitlesOrURLs(t *testing.T) {
	t.Parallel()
	s := &server{b: listBrowser{tabs: []target.Info{
		{ID: "1", Title: "Reset your password", URL: "https://intranet.example/reset?token=s3cret"},
	}}}
	res, err := s.dispatch(t.Context(), "__status", nil)
	if err != nil {
		t.Fatalf("__status: %v", err)
	}
	payload, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"s3cret", "Reset your password", "intranet.example"} {
		if strings.Contains(string(payload), leak) {
			t.Errorf("__status leaked %q into its payload:\n%s", leak, payload)
		}
	}
}

// TestStatusPropagatesADeadDaemonsError: Status must not report `running: true`
// with the status call's error discarded. A daemon holding a dead CDP
// connection answers the socket and fails the call, and swallowing that is one
// of the three unverified claims that let doctor say "ready".
func TestStatusPropagatesADeadDaemonsError(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortTempDir(t), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// Accept, then hang up without answering: exactly what a daemon whose
	// dispatch cannot complete looks like from here.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
			_ = c.Close()
		}
	}()

	if _, err := Status(sock, "127.0.0.1:9222"); err == nil {
		t.Error("Status swallowed the failure and reported the daemon as usable")
	}
}
