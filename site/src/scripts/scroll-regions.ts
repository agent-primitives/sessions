// Keyboard access and names for the two kinds of content that scroll sideways inside the page.
//
// Tables: Starlight renders a markdown table as a block that scrolls when it is wider than the
// column (the API reference's scalar-type table is). A pointer can scroll it; a keyboard cannot
// reach it. While a table overflows it is made focusable, so arrow keys scroll it, the same thing
// Expressive Code does for code blocks.
//
// Code blocks: Expressive Code gives an overflowing <pre> tabindex="0" and role="region" but no
// name. Two unnamed regions on a page are indistinguishable landmarks (axe landmark-unique), and a
// screen reader announces each as just "region". This names a block only while it is a region,
// since aria-label on a plain <pre> is not allowed, and follows Expressive Code as it adds and
// removes the role on resize.

const tables = document.querySelectorAll<HTMLElement>(".sl-markdown-content table");
if (tables.length > 0) {
  const fit = (table: HTMLElement) => {
    if (table.scrollWidth > table.clientWidth) table.setAttribute("tabindex", "0");
    else table.removeAttribute("tabindex");
  };
  const resize = new ResizeObserver((entries) => {
    for (const e of entries) fit(e.target as HTMLElement);
  });
  tables.forEach((t) => resize.observe(t));
}

const blocks = [...document.querySelectorAll<HTMLElement>(".expressive-code pre")];

const nameOf = (pre: HTMLElement, i: number) => {
  const title = pre.closest("figure")?.querySelector(".title")?.textContent?.trim();
  return title ? `Code example ${i + 1}: ${title}` : `Code example ${i + 1}`;
};

const label = (pre: HTMLElement) => {
  const i = blocks.indexOf(pre);
  if (i < 0) return;
  if (pre.getAttribute("role") === "region") pre.setAttribute("aria-label", nameOf(pre, i));
  else pre.removeAttribute("aria-label");
};

if (blocks.length > 0) {
  blocks.forEach(label);
  const observer = new MutationObserver((records) => {
    for (const r of records) label(r.target as HTMLElement);
  });
  for (const pre of blocks) observer.observe(pre, { attributes: true, attributeFilter: ["role"] });
}
