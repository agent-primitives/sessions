// Opens a collapsed <details> when an in-page link points inside it.
//
// The API reference folds its scalar-type table into a <details>, and every field typed `string`,
// `bytes` and so on links to that table's rows. Current Chromium and WebKit open the <details> on
// fragment navigation by themselves; this covers an engine that does not, so a type link never
// scrolls to a row that is still hidden. Opening the ancestors before the browser scrolls means the
// link lands on a visible row.

const targetOf = (hash: string) => {
  if (hash.length < 2) return null;
  const id = decodeURIComponent(hash.slice(1));
  return document.getElementById(id) ?? document.getElementsByName(id)[0] ?? null;
};

const reveal = (hash: string) => {
  const target = targetOf(hash);
  for (let d = target?.closest("details"); d; d = d.parentElement?.closest("details")) {
    d.open = true;
  }
  return target;
};

if (document.querySelector(".sl-markdown-content details")) {
  document.addEventListener("click", (e) => {
    const a = (e.target as Element | null)?.closest<HTMLAnchorElement>('a[href^="#"]');
    if (a) reveal(a.hash);
  });
  window.addEventListener("hashchange", () => reveal(location.hash));
  // A deep link: open first, then scroll, since the browser scrolled before this ran.
  reveal(location.hash)?.scrollIntoView();
}
