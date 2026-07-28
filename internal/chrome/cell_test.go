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

// TestCellAddressingZeroSizeInput covers the grid shape --by cell actually meets
// in the wild and used to fail on: Workday's time grid renders each hour input
// at 0x0 and only gives it a box once its CELL is clicked.
//
// The locator filtered candidate fields by their OWN rect, so on this shape the
// candidate list was empty on every poll, the query returned "not found" until
// the deadline, and the caller got a bare "context deadline exceeded" — on the
// exact grid the flag exists for. Matching through the cell's geometry and
// falling back to the cell element (which is laid out, and whose click mounts
// the input) is what makes this resolve.
func TestCellAddressingZeroSizeInput(t *testing.T) {
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

	// The input is 0x0 until its cell is clicked, exactly like Workday's.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>ZeroCell</title>
<style>
  th,td{width:90px;height:32px;padding:0}
  td input{width:0;height:0;border:0;padding:0}
  td.editing input{width:80px;height:28px}
</style>
<body><table>
<thead><tr><th>Task</th><th>Sun, 7/12</th><th>Mon, 7/13</th><th>Tue, 7/14</th></tr></thead>
<tbody>
<tr><th>Regular</th><td><input id="r_sun" value="0"></td><td><input id="r_mon" value="0"></td><td><input id="r_tue" value="0"></td></tr>
</tbody></table>
<script>
  // Mount the real input box on cell click, as the grid under test does.
  document.querySelectorAll("td").forEach(td => {
    td.addEventListener("mousedown", (e) => {
      // preventDefault keeps the browser from moving focus to the <td> after
      // this handler runs; a td is not focusable, so the default would blur
      // the input straight to BODY. The grid under test keeps focus on the
      // input, so a model that does not would be testing the wrong page.
      e.preventDefault();
      td.classList.add("editing");
      const i = td.querySelector("input");
      i.focus(); i.select();
    });
  });
</script></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	if _, err := b.Fill(ctx, id, "Mon, 7/13", "8", QueryOpts{By: "cell"}); err != nil {
		t.Fatalf("Fill zero-size cell Mon, 7/13: %v", err)
	}
	// Replacement, not append: a cell showing 0 must become 8, never 08.
	for cellID, want := range map[string]string{"r_mon": "8", "r_sun": "0", "r_tue": "0"} {
		got := evalString(ctx, t, b, id, fmt.Sprintf("document.getElementById('%s').value", cellID))
		if got != want {
			t.Errorf("%s = %q, want %q", cellID, got, want)
		}
	}
}
