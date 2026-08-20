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

	b, err := launch(true, tmpProfile(t), 0, "")
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

	b, err := launch(true, tmpProfile(t), 0, "")
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

// TestCellAddressingPrefersGridOverOffGridField covers the second shape --by
// cell met in the wild: a field OUTSIDE the grid whose centre-x happens to fall
// inside one column's tolerance. Workday's global "Search Workday" box sits
// above its Enter Time dialog, its centre inside the Tue column's band; being
// earlier in document order it was picked first, it was under the modal
// overlay, and `fill --by cell "Tue, …"` failed as `occluded` on that column and
// no other, in every week.
//
// The fixture makes the off-grid input hit-testable (not covered), which turns
// the old behaviour into the WORSE failure — the value lands in the search box
// and the grid cell stays 0 — so the assertion is deterministic either way:
// ranking the header's own grid first is what makes the grid cell win.
func TestCellAddressingPrefersGridOverOffGridField(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	// #search is centred over the Tue column (x 310..390 → centre 350) and comes
	// before the table in document order.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>OffGrid</title>
<style>
  body{margin:0}
  #search{position:absolute;left:310px;top:8px;width:80px;height:28px}
  table{position:absolute;left:0;top:80px;border-collapse:collapse}
  th,td{width:100px;height:32px;padding:0;text-align:center}
  td input{width:80px;box-sizing:border-box}
</style>
<body>
<input id="search" placeholder="Search">
<table>
<thead><tr><th>Task</th><th>Sun, 7/12</th><th>Mon, 7/13</th><th>Tue, 7/14</th></tr></thead>
<tbody>
<tr><th>Regular</th><td><input id="r_sun" value="0"></td><td><input id="r_mon" value="0"></td><td><input id="r_tue" value="0"></td></tr>
</tbody></table></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	if _, err := b.Fill(ctx, id, "Tue, 7/14", "8", QueryOpts{By: "cell"}); err != nil {
		t.Fatalf("Fill cell Tue, 7/14: %v", err)
	}
	for cellID, want := range map[string]string{"r_tue": "8", "r_sun": "0", "r_mon": "0", "search": ""} {
		got := evalString(ctx, t, b, id, fmt.Sprintf("document.getElementById('%s').value", cellID))
		if got != want {
			t.Errorf("%s = %q, want %q", cellID, got, want)
		}
	}
}

// TestCellAddressingReResolvesReplacedNode covers the third shape --by cell met
// in the wild: a grid that RE-RENDERS its row after a cell commits. Workday's
// time grid does this after every hour entry, so the input the next fill
// resolved a moment earlier is gone by the time it is measured. Polling that
// node measured 0x0 until the deadline and surfaced as `occluded` — one column
// in five, every row, with no overlay anywhere to dismiss.
//
// The fixture commits on `input`: it covers the row with a "saving" overlay for
// a beat (so the next fill's first measurement is genuinely occluded and it has
// to poll), then rebuilds every input in the row and lifts the overlay. The
// second fill therefore resolves the OLD Tue input, watches it get detached, and
// must re-resolve to the replacement to succeed.
func TestCellAddressingReResolvesReplacedNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Rerender</title>
<style>
  body{margin:0}
  table{position:absolute;left:0;top:40px;border-collapse:collapse}
  th,td{width:100px;height:32px;padding:0;text-align:center}
  td input{width:80px;box-sizing:border-box}
  #veil{position:absolute;left:0;top:0;width:100vw;height:100vh;background:rgba(0,0,0,.05);display:none}
</style>
<body>
<table>
<thead><tr><th>Task</th><th>Sun, 7/12</th><th>Mon, 7/13</th><th>Tue, 7/14</th></tr></thead>
<tbody>
<tr id="row"><th>Regular</th><td><input id="r_sun" value="0"></td><td><input id="r_mon" value="0"></td><td><input id="r_tue" value="0"></td></tr>
</tbody></table>
<div id="veil"></div>
<script>
  // Commit = veil the grid, then rebuild the row's inputs (new elements, same
  // ids and values) and lift the veil — the grid under test's shape.
  const arm = () => document.querySelectorAll("#row input").forEach(i => i.addEventListener("input", commit, {once: true}));
  const commit = () => {
    document.getElementById("veil").style.display = "block";
    setTimeout(() => {
      document.querySelectorAll("#row input").forEach(old => {
        const fresh = document.createElement("input");
        fresh.id = old.id; fresh.value = old.value;
        old.replaceWith(fresh);
      });
      arm();
      document.getElementById("veil").style.display = "none";
    }, 400);
  };
  arm();
</script></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	if _, err := b.Fill(ctx, id, "Mon, 7/13", "8", QueryOpts{By: "cell"}); err != nil {
		t.Fatalf("Fill Mon, 7/13: %v", err)
	}
	// Straight into the next cell while the row is mid-commit. Bounded so the
	// failure mode (polling a detached node until the deadline) fails the test
	// in seconds, not the suite's minute.
	fctx, fcancel := context.WithTimeout(ctx, 8*time.Second)
	defer fcancel()
	if _, err := b.Fill(fctx, id, "Tue, 7/14", "8", QueryOpts{By: "cell"}); err != nil {
		t.Fatalf("Fill Tue, 7/14 across the row re-render: %v", err)
	}
	for cellID, want := range map[string]string{"r_mon": "8", "r_tue": "8", "r_sun": "0"} {
		got := evalString(ctx, t, b, id, fmt.Sprintf("document.getElementById('%s').value", cellID))
		if got != want {
			t.Errorf("%s = %q, want %q", cellID, got, want)
		}
	}
}
