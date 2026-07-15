package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --by cell resolves a grid input by its column header (and optional row header),
// so a caller fills the right day column without mapping inputs by coordinate.
func TestCellAddressing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Cell</title>
<style>th,td{width:80px}</style>
<body><table>
<thead><tr><th>Task</th><th>Sun, 7/12</th><th>Mon, 7/13</th><th>Tue, 7/14</th></tr></thead>
<tbody>
<tr><th>Regular</th><td><input id="r_sun" value="0"></td><td><input id="r_mon" value="0"></td><td><input id="r_tue" value="0"></td></tr>
<tr><th>Overtime</th><td><input id="o_sun" value="0"></td><td><input id="o_mon" value="0"></td><td><input id="o_tue" value="0"></td></tr>
</tbody></table></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// Single header targets the column; with two rows, the row header disambiguates.
	if _, err := b.Fill(ctx, id, "Regular|Mon, 7/13", "8", QueryOpts{By: "cell"}); err != nil {
		t.Fatalf("Fill cell Regular/Mon: %v", err)
	}
	if _, err := b.Fill(ctx, id, "Overtime|Tue, 7/14", "3", QueryOpts{By: "cell"}); err != nil {
		t.Fatalf("Fill cell Overtime/Tue: %v", err)
	}

	checks := map[string]string{
		"r_mon": "8", "o_tue": "3",
		"r_sun": "0", "r_tue": "0", "o_sun": "0", "o_mon": "0",
	}
	for cellID, want := range checks {
		got := evalString(ctx, t, b, id, fmt.Sprintf("document.getElementById('%s').value", cellID))
		if got != want {
			t.Errorf("%s = %q, want %q", cellID, got, want)
		}
	}
}
