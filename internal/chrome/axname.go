package chrome

// The JS accessible-name computation shared by every DOM-side path.
//
// Chrome throttles the accessibility tree on a backgrounded tab, so the verbs
// that must keep working there (`--by name` resolution, `find`'s fallback)
// compute a simplified accessible name in JS instead. There is exactly one
// definition of that computation, here, because there was briefly more than
// one: a second copy silently lost the `<input type="submit" value="Sign In">`
// case, so the same button resolved by name and was invisible to search.
//
// Callers concatenate this fragment ahead of their own IIFE body. It defines
// `norm`, `visible`, `roleOf`, `textRoles`, and `accName`, declares no globals,
// and contains no `%` verbs — so a caller may still run the combined string
// through fmt.Sprintf for its own arguments.
const axNameHelpersJS = `
  const norm = s => (s || "").replace(/\s+/g, " ").trim();
  const visible = el => {
    if (el.getAttribute("aria-hidden") === "true") return false;
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none") return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const roleOf = el => {
    const ex = el.getAttribute("role"); if (ex) return ex;
    const tag = el.tagName.toLowerCase();
    if (tag === "button") return "button";
    if (tag === "a" && el.hasAttribute("href")) return "link";
    if (tag === "select") return "combobox";
    if (tag === "textarea") return "textbox";
    if (/^h[1-6]$/.test(tag)) return "heading";
    if (tag === "input") {
      const ty = (el.getAttribute("type") || "text").toLowerCase();
      if (["button", "submit", "reset"].includes(ty)) return "button";
      if (ty === "checkbox") return "checkbox";
      if (ty === "radio") return "radio";
      return "textbox";
    }
    return "";
  };
  const textRoles = ["button", "link", "heading", "option", "menuitem", "menuitemradio", "menuitemcheckbox", "tab", "treeitem", "cell", "columnheader", "rowheader"];
  const accName = el => {
    const al = el.getAttribute("aria-label"); if (al) return al;
    const lb = el.getAttribute("aria-labelledby");
    if (lb) {
      const t = lb.split(/\s+/).map(id => { const e = document.getElementById(id); return e ? e.textContent : ""; }).join(" ");
      if (norm(t)) return t;
    }
    if (el.id) { try { const lab = document.querySelector('label[for="' + CSS.escape(el.id) + '"]'); if (lab && norm(lab.textContent)) return lab.textContent; } catch (e) {} }
    const wrap = el.closest("label"); if (wrap && norm(wrap.textContent)) return wrap.textContent;
    if (textRoles.includes(roleOf(el)) && norm(el.textContent)) return el.textContent;
    const ph = el.getAttribute("placeholder"); if (ph) return ph;
    const ti = el.getAttribute("title"); if (ti) return ti;
    const alt = el.getAttribute("alt"); if (alt) return alt;
    if (el.tagName === "INPUT" && ["button", "submit", "reset"].includes((el.getAttribute("type") || "").toLowerCase())) {
      const v = el.getAttribute("value"); if (v) return v;
    }
    return "";
  };

  // Chrome MASKS a password field's value in the accessibility tree (it reports
  // bullets, not the characters). A DOM-side read sees the literal text the
  // user typed, so it masks too — otherwise a fallback path would leak a typed
  // password that the primary path never exposes. Same masking, same length,
  // so both paths agree on the field's shape.
  const axValueOf = el => {
    const ty = (el.getAttribute("type") || "").toLowerCase();
    if (ty === "hidden") return "";
    const v = norm(el.value || "");
    if (ty === "password") return "•".repeat(v.length);
    return v;
  };
`
