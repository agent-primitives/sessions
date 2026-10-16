// Opens a collapsed <details> when an in-page link points inside it.
//
// The API reference folds its scalar-type table into a <details>, and every field typed `string`,
// `bytes` and so on links to that table's rows. Current Chromium and WebKit open the <details> on
// fragment navigation by themselves; this covers an engine that does not, so a type link never
// scrolls to a row that is still hidden. Opening the ancestors before the browser scrolls means the
// link lands on a visible row.

const targetOf = (hash: string) => {
  if (hash.length < 2) return null;
  let id: string;
  try {
    id = decodeURIComponent(hash.slice(1));
  } catch {
    // A malformed escape such as #%E0%A4%A cannot name an element on this page.
    return null;
  }
  return document.getElementById(id) ?? document.getElementsByName(id)[0] ?? null;
};

// Opens every closed <details> around the hash target. Returns the target only when it opened one,
// so the hashchange and page-load paths re-scroll just the links the browser could not have
// scrolled to. The click path ignores it: it runs before navigation, so the browser scrolls after.
const reveal = (hash: string) => {
  const target = targetOf(hash);
  let opened = false;
  for (let d = target?.closest("details"); d; d = d.parentElement?.closest("details")) {
    if (!d.open) {
      d.open = true;
      opened = true;
    }
  }
  return opened ? target : null;
};

if (document.querySelector(".sl-markdown-content details")) {
  document.addEventListener("click", (e) => {
    const a = (e.target as Element | null)?.closest<HTMLAnchorElement>('a[href^="#"]');
    if (a) reveal(a.hash);
  });
  // Back, forward and an edited hash fire hashchange after the browser scrolled, so re-scroll once
  // the row is visible.
  window.addEventListener("hashchange", () => reveal(location.hash)?.scrollIntoView());
  // A deep link into a closed <details>: the browser scrolled before this ran, so scroll again once
  // the row is visible.
  reveal(location.hash)?.scrollIntoView();
}
