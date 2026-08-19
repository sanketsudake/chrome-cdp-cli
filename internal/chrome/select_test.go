package chrome

// Integration tests for the `select` verb against a MANAGED headless Chrome and
// an httptest fixture that reproduces the Workday behaviours root-caused in
// .scratch/cdp-ergonomics/e3-rootcause.md: a portal popup that opens only on a
// real pointer sequence (delegated capture-phase handler), mounts collapsed (a
// zero-scale transform) then animates open, a cascade category that drills via a
// right-edge chevron, and options that live in a detached subtree.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// cascadePromptHTML is a minimal reproduction of the Time Type cascade prompt.
// The popup opens on a *trusted* mousedown (delegated on document, capture
// phase), starts at transform:scale(0) and only un-collapses after a tick, and
// its category row drills into leaves via a chevron on the right edge — so a
// naive node-click (one box read, no settle) at the row centre would miss.
const cascadePromptHTML = `<!doctype html><title>Cascade</title>
<style>
  body { margin:0; font-family:sans-serif; }
  #field { position:fixed; left:40px; top:40px; width:200px; }
  #ttinput { width:200px; height:30px; box-sizing:border-box; }
  #pop { position:fixed; left:40px; top:80px; width:240px;
         transform:scale(0); transform-origin:top left; background:#fff; border:1px solid #ccc; }
  #pop.open { transform:scale(1); }
  .row { display:flex; align-items:center; height:30px; padding:0 6px; }
  .lbl { flex:1; }
  .chev { width:22px; height:22px; text-align:center; }
</style>
<body>
<div id="field"><input id="ttinput" role="textbox" aria-label="Time Type" readonly value="Select one"></div>
<div id="pop" role="listbox" aria-hidden="true"></div>
<script>
  const field = document.getElementById('field');
  const input = document.getElementById('ttinput');
  const pop = document.getElementById('pop');
  const catRows = () => {
    pop.innerHTML =
      '<div class="row" role="option" data-cat="ppt"><span class="lbl">Project Plan Tasks</span><span class="chev">&rsaquo;</span></div>' +
      '<div class="row" role="option" data-cat="mru"><span class="lbl">Most Recently Used</span><span class="chev">&rsaquo;</span></div>';
  };
  // Open ONLY on a trusted mousedown inside the field (delegated, capture phase),
  // and un-collapse a tick later so the open animation must be waited out.
  document.addEventListener('mousedown', e => {
    if (!e.isTrusted) return;
    if (!field.contains(e.target)) return;
    catRows();
    pop.setAttribute('aria-hidden', 'false');
    requestAnimationFrame(() => setTimeout(() => pop.classList.add('open'), 120));
  }, true);
  // Clicking a category ROW drills into it; clicking a leaf ROW selects it —
  // matching real Workday (the row body drives, not a chevron).
  document.addEventListener('click', e => {
    if (!e.isTrusted) return;
    const row = e.target.closest('.row');
    if (!row) return;
    if (row.dataset.cat === 'ppt') {
      pop.innerHTML =
        '<div class="row leaf" role="option"><span class="lbl">Other: Thing</span></div>' +
        '<div class="row leaf" role="option"><span class="lbl">ShiftLeft: Qwiet</span></div>';
      return;
    }
    if (row.classList.contains('leaf')) {
      const txt = row.querySelector('.lbl').textContent;
      input.value = txt;
      pop.classList.remove('open');
      pop.setAttribute('aria-hidden', 'true');
      window.__selected = txt;
    }
  }, true);
</script>`

func TestSelectCascadePrompt(t *testing.T) {
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
		fmt.Fprint(w, cascadePromptHTML)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// The whole cascade — open the prompt, drill "Project Plan Tasks", pick the
	// leaf — in one call. A plain click cannot do this (spec §E3 blocker).
	res, err := b.Select(ctx, id, "Time Type", "Project Plan Tasks > ShiftLeft: Qwiet",
		SelectOpts{Query: QueryOpts{By: "name", Role: "textbox"}})
	if err != nil {
		t.Fatalf("Select cascade: %v", err)
	}
	if res["selected"] != "Project Plan Tasks > ShiftLeft: Qwiet" {
		t.Errorf("selected = %v, want the full path", res["selected"])
	}

	// The fixture records the chosen leaf and reflects it in the field value.
	got, err := b.Eval(ctx, id, "window.__selected", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v := got.(map[string]any)["value"]; v != "ShiftLeft: Qwiet" {
		t.Errorf("window.__selected = %v, want ShiftLeft: Qwiet", v)
	}
	val, err := b.Eval(ctx, id, "document.getElementById('ttinput').value", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval value: %v", err)
	}
	if v := val.(map[string]any)["value"]; v != "ShiftLeft: Qwiet" {
		t.Errorf("field value = %v, want ShiftLeft: Qwiet", v)
	}
}

// An option path whose final segment resolves to a category (it drills instead of
// selecting) must be reported as a failure — never a false success — so a caller
// doesn't proceed against no selection.
func TestSelectRejectsIncompletePath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, cascadePromptHTML)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// "Project Plan Tasks" is a category (drills to leaves) — selecting it as the
	// terminal option commits nothing, so Select must error.
	_, err = b.Select(ctx, id, "Time Type", "Project Plan Tasks",
		SelectOpts{Query: QueryOpts{By: "name", Role: "textbox"}})
	if err == nil {
		t.Fatal("Select of a category-only path returned nil error, want a not-committed failure")
	}
}

func TestSelectNativeSelect(t *testing.T) {
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
		fmt.Fprint(w, `<!doctype html><title>Native</title><body>
<select aria-label="Country" onchange="window.__country=this.value">
  <option value="in">India</option>
  <option value="us">United States</option>
</select></body>`)
	}))
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	res, err := b.Select(ctx, id, "Country", "United States",
		SelectOpts{Query: QueryOpts{By: "name", Role: "combobox"}})
	if err != nil {
		t.Fatalf("Select native: %v", err)
	}
	if res["widget"] != "native-select" {
		t.Errorf("widget = %v, want native-select", res["widget"])
	}
	got, err := b.Eval(ctx, id, "window.__country", EvalOpts{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v := got.(map[string]any)["value"]; v != "us" {
		t.Errorf("window.__country = %v, want us", v)
	}
}

func firstTab(ctx context.Context, t *testing.T, b Browser) string {
	t.Helper()
	tabs, err := b.List(ctx)
	if err != nil || len(tabs) == 0 {
		t.Fatalf("List: %v", err)
	}
	return tabs[0].ID
}
