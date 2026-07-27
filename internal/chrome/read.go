package chrome

// Page-reading verbs (RFC-0010): `text` (optionally --article) and `eval`
// (optionally --await). Both take an option struct so the reading surface can
// grow without another interface method.
//
// The two features are independent; they share this file because they share the
// same friction — getting useful data out of a page without ceremony.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// DefaultArticleMinChars is the `--min-chars` default: below this many extracted
// characters, `text --article` reports `extracted: false` and returns the full
// page text rather than a plausible-looking fragment.
const DefaultArticleMinChars = 250

// JSError is a JavaScript exception or promise rejection raised by an evaluated
// expression, kept distinct from a transport failure so the CLI can put the
// rejection's message and stack in the envelope's error details.
//
// It exists because of the single most important correctness point in RFC-0010:
// an awaited promise that REJECTS must surface as an error, never as a
// successful envelope carrying an odd-looking value.
type JSError struct {
	Message string
	Stack   string
}

func (e *JSError) Error() string { return e.Message }

// JSException reports whether err is a JavaScript exception/rejection, and
// returns it. Callers use it to enrich the error envelope; everything else
// treats a JSError as an ordinary CDP error.
func JSException(err error) (*JSError, bool) {
	var e *JSError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// jsError converts CDP exception details into a JSError. Chrome reports a
// rejection the same way it reports a throw — via exceptionDetails — so both
// land here, and the caller cannot mistake either for a value.
func jsError(exc *cdpruntime.ExceptionDetails) *JSError {
	e := &JSError{Message: strings.TrimSpace(exc.Text)}
	if exc.Exception != nil {
		if d := strings.TrimSpace(exc.Exception.Description); d != "" {
			// Description is "Error: boom\n    at <anonymous>:1:11": the first
			// line is the message, the rest is the stack.
			msg, stack, _ := strings.Cut(d, "\n")
			e.Message, e.Stack = strings.TrimSpace(msg), strings.TrimSpace(stack)
		} else if v := exc.Exception.Value; len(v) > 0 {
			// A rejection of a non-Error value (Promise.reject("nope")) has no
			// description — report the value itself rather than a bare "Uncaught".
			e.Message = strings.TrimSpace(string(v))
		}
	}
	if e.Stack == "" {
		e.Stack = stackText(exc.StackTrace)
	}
	if e.Message == "" {
		e.Message = "javascript exception"
	}
	return e
}

// stackText renders a CDP stack trace as the familiar "at fn (url:line:col)"
// lines, so `error.details.stack` reads the way a JS developer expects.
func stackText(st *cdpruntime.StackTrace) string {
	if st == nil {
		return ""
	}
	var b strings.Builder
	for _, f := range st.CallFrames {
		name := f.FunctionName
		if name == "" {
			name = "<anonymous>"
		}
		fmt.Fprintf(&b, "at %s (%s:%d:%d)\n", name, f.URL, f.LineNumber+1, f.ColumnNumber+1)
	}
	return strings.TrimSpace(b.String())
}

// Eval evaluates expr in the tab and returns {"value": …}.
//
// The DEFAULT path is untouched by RFC-0010 (Open Question 1): a plain
// expression evaluation in the page's main world. `--await` is opt-in because
// replMode changes how bare object literals and let/const re-declaration behave,
// and flipping that silently would break existing scripts.
func (c *CDP) Eval(ctx context.Context, id, expr string, opts EvalOpts) (any, error) {
	if opts.Await {
		return c.evalAwait(ctx, id, expr)
	}
	var res json.RawMessage
	if err := c.run(ctx, id, chromedp.Evaluate(expr, &res)); err != nil {
		// chromedp hands the exception details back as the error itself. Keep its
		// message verbatim (that is the long-standing contract) but carry the
		// stack alongside, so both eval modes can report one.
		var exc *cdpruntime.ExceptionDetails
		if errors.As(err, &exc) {
			return nil, &JSError{Message: err.Error(), Stack: stackText(exc.StackTrace)}
		}
		return nil, err
	}
	var v any
	_ = json.Unmarshal(res, &v)
	return map[string]any{"value": v}, nil
}

// evalAwait evaluates expr with DevTools console semantics: `awaitPromise` so a
// top-level `await` resolves, and `replMode` so a statement list yields its final
// expression's value.
//
// A never-settling promise is bounded by the caller's context (c.run applies the
// command deadline), and cancelling a pending Runtime.evaluate leaves the
// connection usable — the next command on the same tab succeeds.
func (c *CDP) evalAwait(ctx context.Context, id, expr string) (any, error) {
	var v any
	err := c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		res, exc, err := cdpruntime.Evaluate(expr).
			WithAwaitPromise(true).
			WithReplMode(true).
			WithReturnByValue(true).
			Do(actx)
		if err != nil {
			return err
		}
		if exc != nil {
			return jsError(exc)
		}
		if res != nil && len(res.Value) > 0 {
			_ = json.Unmarshal([]byte(res.Value), &v)
		}
		return nil
	}))
	if err != nil {
		return nil, err
	}
	// `awaited` records which path ran, so a caller reading the envelope knows
	// whether REPL semantics were in force.
	return map[string]any{"value": v, "awaited": true}, nil
}

// Text returns the visible text of selector, or — with opts.Article — the page's
// main readable content with navigation, footers, and cookie banners dropped.
func (c *CDP) Text(ctx context.Context, id, selector string, opts TextOpts) (map[string]any, error) {
	if !opts.Article {
		var text string
		if err := c.run(ctx, id, chromedp.Text(selector, &text, query(selector, opts.Query)...)); err != nil {
			return nil, err
		}
		return map[string]any{"text": text}, nil
	}
	minChars := opts.MinChars
	if minChars <= 0 {
		minChars = DefaultArticleMinChars
	}
	ex, err := c.extract(ctx, id, opts.Markdown)
	if err != nil {
		return nil, err
	}
	return articleResult(ex, minChars, opts.Markdown), nil
}

// extraction is the raw output of the injected extractor: the candidate main
// content, the page's full visible text to measure it against, and the metadata
// read from the document head.
type extraction struct {
	Article string `json:"article"`
	Full    string `json:"full"`
	Title   string `json:"title"`
	Byline  string `json:"byline"`
}

// extract runs the article extractor in an ISOLATED WORLD. The isolated world is
// not cosmetic: it shares the DOM but has its own global object, so the
// extractor leaves nothing on `window` and page script can neither observe nor
// monkey-patch it.
func (c *CDP) extract(ctx context.Context, id string, markdown bool) (extraction, error) {
	optsJSON, err := json.Marshal(map[string]any{"markdown": markdown})
	if err != nil {
		return extraction{}, err
	}
	// Replace (not Sprintf): the extractor is full of % and { } that a format
	// string would mangle.
	expr := strings.Replace(articleJS, "__OPTS__", string(optsJSON), 1)

	var ex extraction
	err = c.run(ctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		tree, err := page.GetFrameTree().Do(actx)
		if err != nil {
			return err
		}
		world, err := page.CreateIsolatedWorld(tree.Frame.ID).
			WithWorldName("chrome-cdp-article").
			WithGrantUniveralAccess(false).
			Do(actx)
		if err != nil {
			return err
		}
		res, exc, err := cdpruntime.Evaluate(expr).
			WithContextID(world).
			WithReturnByValue(true).
			Do(actx)
		if err != nil {
			return err
		}
		if exc != nil {
			return jsError(exc)
		}
		if res == nil || len(res.Value) == 0 {
			return errors.New("article extraction returned no value")
		}
		return json.Unmarshal([]byte(res.Value), &ex)
	}))
	return ex, err
}

// articleResult applies the min-chars threshold to a raw extraction and builds
// the reported fields. It is pure — no browser — because the heuristic's
// *reporting* (ratio, chars, the extracted/failed decision) is the part callers
// depend on to detect a bad extraction, and it must be verifiable without a
// renderer even though the scoring itself can only be tested end to end.
//
// A low-confidence extraction is deliberately NOT an error: the caller gets the
// full page text plus an honest `extracted: false` and a reason, because failing
// a read that did return usable text would be unhelpful (RFC-0010 US-5).
func articleResult(ex extraction, minChars int, markdown bool) map[string]any {
	total := utf8.RuneCountInString(ex.Full)
	kept := utf8.RuneCountInString(ex.Article)

	format := "text"
	if markdown {
		format = "markdown"
	}
	out := map[string]any{
		"title":       ex.Title,
		"byline":      ex.Byline,
		"total_chars": total,
		"extracted":   kept >= minChars,
		"format":      format,
	}

	text := ex.Article
	if kept < minChars {
		text = ex.Full
		out["article_chars"] = kept
		out["reason"] = fmt.Sprintf(
			"extraction kept only %d characters, below the %d-character minimum (--min-chars); returning the full page text",
			kept, minChars)
	}
	chars := utf8.RuneCountInString(text)
	out["text"] = text
	out["chars"] = chars
	out["ratio"] = keptRatio(chars, total)
	out["excerpt"] = excerpt(text, 200)
	return out
}

// keptRatio is chars ÷ total, rounded to three places so the envelope carries a
// readable number rather than a float artefact. An empty page is 0, not NaN.
func keptRatio(chars, total int) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(chars)/float64(total)*1000) / 1000
}

// excerpt returns the opening of the text — the first paragraph, capped at max
// runes — so a caller can eyeball whether extraction found the right subtree
// without printing the whole thing.
func excerpt(text string, max int) string {
	first := text
	if i := strings.Index(first, "\n\n"); i > 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(strings.ReplaceAll(first, "\n", " "))
	if utf8.RuneCountInString(first) <= max {
		return first
	}
	r := []rune(first)[:max]
	// Trim back to a word boundary so the excerpt does not end mid-word.
	if i := strings.LastIndex(string(r), " "); i > max/2 {
		return strings.TrimSpace(string(r)[:i]) + "…"
	}
	return strings.TrimSpace(string(r)) + "…"
}

// articleJS is the injected extractor: one self-contained IIFE, evaluated in an
// isolated world, that returns {article, full, title, byline}. __OPTS__ is
// replaced with a JSON object of the extraction options.
//
// Per RFC-0010 Open Question 2 this is a small, deliberately conservative
// Readability-style implementation rather than a hand-tuned one: the block
// scoring, the class/id regexes, and the sibling-append step follow Mozilla's
// Readability, so the behaviour is the well-trodden one and future maintenance
// is "resync with upstream", not "invent more heuristics".
//
// THE CORRECTNESS PROPERTY THAT MATTERS MOST: it never mutates the live DOM. It
// clones document.body and strips, scores, and serialises the CLONE. A reading
// verb that changed the page under a running automation would corrupt every
// other command in the same session. The only live-DOM reads are innerText,
// textContent, and head metadata.
//
// JS backticks are written as \x60 so this can live in a Go raw string literal.
const articleJS = `
(() => {
  "use strict";
  const OPTS = __OPTS__;                       // {markdown: bool}
  const MD = !!OPTS.markdown;
  const FENCE = "\x60\x60\x60";

  const norm = (s) => (s || "")
    .replace(/[ \t ]+/g, " ")
    .replace(/ *\n */g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
  const flat = (s) => (s || "").replace(/\s+/g, " ").trim();
  // tidy joins already-flattened blocks. Unlike norm it must NOT collapse runs
  // of spaces: the blocks include preformatted code, whose indentation is content.
  const tidy = (s) => (s || "")
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();

  // ---------------------------------------------------------------- signals

  // Landmarks and roles that are furniture by definition. RFC-0010 names nav,
  // header, footer, aside, [role=navigation] and [aria-hidden=true]; the rest
  // are the same idea (interactive chrome and non-content embeds).
  const FURNITURE = [
    "script", "style", "noscript", "template", "link", "meta", "svg", "canvas",
    "iframe", "object", "embed", "video", "audio", "picture", "source",
    "nav", "header", "footer", "aside", "form", "button", "input", "select",
    "textarea", "dialog", "menu",
    "[role=navigation]", "[role=banner]", "[role=contentinfo]",
    "[role=complementary]", "[role=search]", "[role=dialog]",
    "[role=alertdialog]", "[role=menu]", "[role=menubar]", "[role=toolbar]",
    "[aria-hidden=true]", "[hidden]"
  ].join(",");

  // Mozilla Readability's class/id regexes, extended with the modern furniture
  // its list predates (cookie/consent/paywall/newsletter).
  const UNLIKELY = /-ad-|banner|breadcrumb|combx|comment|community|consent|cookie|cover-wrap|disqus|extra|footer|gdpr|legends|masthead|menu|newsletter|paywall|popup|promo|related|remark|replies|rss|shoutbox|sidebar|signup|skyscraper|social|sponsor|subscribe|supplemental|survey|toolbar|ad-break|agegate|pagination|pager|share/i;
  const MAYBE    = /and|article|body|column|content|main|shadow/i;
  const NEGATIVE = /-ad-|hidden|banner|combx|comment|com-|contact|footer|gdpr|masthead|media|meta|outbrain|promo|related|scroll|share|shoutbox|sidebar|skyscraper|sponsor|shopping|tags|widget/i;
  const POSITIVE = /article|body|content|entry|hentry|h-entry|main|page|pagination|post|text|blog|story/i;

  const BLOCK = new Set(["ADDRESS","ARTICLE","ASIDE","BLOCKQUOTE","DETAILS","DIV",
    "DL","DD","DT","FIELDSET","FIGCAPTION","FIGURE","FOOTER","FORM","H1","H2",
    "H3","H4","H5","H6","HEADER","HGROUP","HR","LI","MAIN","NAV","OL","P","PRE",
    "SECTION","TABLE","TBODY","TD","TFOOT","TH","THEAD","TR","UL"]);

  const sig = (el) => {
    const cls = typeof el.className === "string" ? el.className : "";
    return cls + " " + (el.id || "");
  };
  const isMain = (el) =>
    el.tagName === "ARTICLE" || el.tagName === "MAIN" ||
    el.getAttribute("role") === "main";

  // ------------------------------------------------------ live, read-only

  const body = document.body;
  const empty = { article: "", full: "", title: "", byline: "" };
  if (!body) return empty;

  // innerText honours CSS visibility, so this is the same baseline a plain
  // ` + "`text`" + ` read would have returned — the denominator for ratio.
  const full = norm(body.innerText || body.textContent || "");

  const attrOf = (sel, attr) => {
    const el = document.querySelector(sel);
    return el ? flat(el.getAttribute(attr) || "") : "";
  };
  const textOf = (sel) => {
    const el = document.querySelector(sel);
    return el ? flat(el.textContent || "") : "";
  };

  const title =
    attrOf('meta[property="og:title"]', "content") ||
    attrOf('meta[name="twitter:title"]', "content") ||
    textOf("article h1") || textOf("h1") || flat(document.title);

  const byline =
    attrOf('meta[name="author"]', "content") ||
    attrOf('meta[property="article:author"]', "content") ||
    textOf('[itemprop="author"] [itemprop="name"]') ||
    textOf('[itemprop="author"]') ||
    textOf('[rel="author"]') ||
    textOf(".byline") || textOf(".author");

  // ------------------------------------------------------- clone and strip
  //
  // Everything below operates on this detached copy. The live document is never
  // written to.
  const root = body.cloneNode(true);
  root.querySelectorAll(FURNITURE).forEach((el) => el.remove());
  for (const el of Array.from(root.querySelectorAll("*"))) {
    const s = sig(el);
    if (!s.trim()) continue;
    if (UNLIKELY.test(s) && !MAYBE.test(s) && !isMain(el)) el.remove();
  }

  // ------------------------------------------------------------- scoring

  const linkDensity = (el) => {
    const len = flat(el.textContent).length;
    if (!len) return 0;
    let linked = 0;
    el.querySelectorAll("a").forEach((a) => { linked += flat(a.textContent).length; });
    return Math.min(1, linked / len);
  };

  const baseScore = (el) => {
    let s = 0;
    switch (el.tagName) {
      case "ARTICLE": case "MAIN": s += 10; break;
      case "SECTION": case "DIV": s += 5; break;
      case "PRE": case "TD": case "BLOCKQUOTE": s += 3; break;
      case "ADDRESS": case "OL": case "UL": case "DL": case "DD": case "DT":
      case "LI": s -= 3; break;
      case "H1": case "H2": case "H3": case "H4": case "H5": case "H6":
      case "TH": s -= 5; break;
    }
    const s2 = sig(el);
    if (POSITIVE.test(s2)) s += 25;
    if (NEGATIVE.test(s2)) s -= 25;
    if (isMain(el)) s += 25;
    return s;
  };

  // A "paragraph" is a text-bearing leaf block: the tags that always are, plus
  // a div/section that holds no block children (the shape a lot of CMS output
  // has, and the reason a p-only scan misses whole articles).
  const LEAFY = new Set(["P", "PRE", "BLOCKQUOTE", "TD", "LI", "FIGCAPTION"]);
  const paragraphs = Array.from(root.querySelectorAll("p, pre, blockquote, td, li, figcaption, div, section"))
    .filter((el) => LEAFY.has(el.tagName) ||
      !Array.from(el.children).some((c) => BLOCK.has(c.tagName)));

  const scores = new Map();
  for (const node of paragraphs) {
    const text = flat(node.textContent);
    if (text.length < 25) continue;
    // Readability's content score: a base point, one per comma (a proxy for
    // prose), and up to three for sheer length.
    let contentScore = 1;
    contentScore += (text.match(/[,，、]/g) || []).length;
    contentScore += Math.min(Math.floor(text.length / 100), 3);
    let anc = node.parentElement;
    for (let level = 0; anc && level < 3; level++, anc = anc.parentElement) {
      if (!scores.has(anc)) scores.set(anc, baseScore(anc));
      const divider = level === 0 ? 1 : (level === 1 ? 2 : level * 3);
      scores.set(anc, scores.get(anc) + contentScore / divider);
    }
  }

  // Link density is the tie-breaker that keeps a link farm from beating prose:
  // a nav-heavy container scores well on raw length and loses it all here.
  let top = null, topScore = -Infinity;
  scores.forEach((s, el) => {
    const finalScore = s * (1 - linkDensity(el));
    scores.set(el, finalScore);
    if (finalScore > topScore) { topScore = finalScore; top = el; }
  });
  if (!top) return { article: "", full: full, title: title, byline: byline };

  // Readability's sibling append: an article split across sibling blocks (lead
  // paragraph outside the main div, say) would otherwise lose its opening.
  const threshold = Math.max(10, topScore * 0.2);
  let chosen = [top];
  const parent = top.parentElement;
  if (parent && parent !== root) {
    chosen = Array.from(parent.children).filter((sib) => {
      if (sib === top) return true;
      const s = scores.has(sib) ? scores.get(sib) : -Infinity;
      if (s >= threshold) return true;
      return sib.tagName === "P" &&
        flat(sib.textContent).length > 80 && linkDensity(sib) < 0.25;
    });
  }

  // ----------------------------------------------------------- serialising

  const inline = (node) => {
    if (node.nodeType === 3) return node.nodeValue.replace(/\s+/g, " ");
    if (node.nodeType !== 1) return "";
    const kids = () => Array.from(node.childNodes).map(inline).join("");
    if (node.tagName === "BR") return "\n";
    if (!MD) return kids();
    switch (node.tagName) {
      case "A": {
        const t = kids().trim();
        const href = node.getAttribute("href") ? node.href : "";
        return (t && href) ? "[" + t + "](" + href + ")" : t;
      }
      case "CODE": case "KBD": case "SAMP": {
        const t = kids().trim();
        return t ? "\x60" + t + "\x60" : "";
      }
      case "STRONG": case "B": {
        const t = kids().trim();
        return t ? "**" + t + "**" : "";
      }
      case "EM": case "I": {
        const t = kids().trim();
        return t ? "*" + t + "*" : "";
      }
      case "IMG": case "PICTURE": return "";   // embedded media: out of scope
      default: return kids();
    }
  };

  const renderList = (node, depth) => {
    const ordered = node.tagName === "OL";
    const pad = "  ".repeat(depth);
    const lines = [];
    let n = 1;
    for (const li of Array.from(node.children)) {
      if (li.tagName !== "LI") continue;
      const nested = [];
      let own = "";
      for (const kid of Array.from(li.childNodes)) {
        if (kid.nodeType === 1 && (kid.tagName === "UL" || kid.tagName === "OL")) {
          nested.push(renderList(kid, depth + 1));
        } else {
          own += inline(kid);
        }
      }
      const text = flat(own);
      const marker = MD ? (ordered ? n + ". " : "- ") : "";
      let line = pad + marker + text;
      const sub = nested.filter(Boolean).join("\n");
      if (sub) line += "\n" + sub;
      if (text || sub) lines.push(line);
      n++;
    }
    return lines.join("\n");
  };

  const render = (node, depth) => {
    if (node.nodeType === 3) return flat(node.nodeValue);
    if (node.nodeType !== 1) return "";
    const tag = node.tagName;
    if (tag === "BR") return "";
    if (tag === "HR") return MD ? "---" : "";
    if (/^H[1-6]$/.test(tag)) {
      const t = flat(inline(node));
      if (!t) return "";
      return MD ? "#".repeat(Number(tag[1])) + " " + t : t;
    }
    if (tag === "P" || tag === "FIGCAPTION") return flat(inline(node));
    if (tag === "PRE") {
      const t = node.textContent.replace(/\s+$/, "");
      if (!t.trim()) return "";
      return MD ? FENCE + "\n" + t + "\n" + FENCE : t;
    }
    if (tag === "BLOCKQUOTE") {
      const inner = Array.from(node.childNodes).map((n) => render(n, depth))
        .filter(Boolean).join("\n\n").trim();
      if (!inner) return "";
      return MD ? inner.split("\n").map((l) => "> " + l).join("\n") : inner;
    }
    if (tag === "UL" || tag === "OL") return renderList(node, depth);
    // Tables are out of scope for --markdown (RFC-0010): keep the text so no
    // content is silently lost, but do not pretend to render a markdown table.
    if (tag === "TABLE") return norm(node.textContent);
    if (BLOCK.has(tag)) {
      const hasBlockChild = Array.from(node.children).some((c) => BLOCK.has(c.tagName));
      if (!hasBlockChild) return flat(inline(node));
      return Array.from(node.childNodes).map((n) => render(n, depth))
        .filter(Boolean).join("\n\n");
    }
    return flat(inline(node));
  };

  const article = tidy(chosen.map((el) => render(el, 0)).filter(Boolean).join("\n\n"));
  return { article: article, full: full, title: title, byline: byline };
})()
`
