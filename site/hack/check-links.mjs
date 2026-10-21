// Checks the built site for internal links that do not resolve.
//
// The docs are written for GitHub, where links are relative paths like (concepts.md#anchor).
// sync-docs.mjs rewrites those into site routes, and a regex rewrite is exactly the kind of thing
// that breaks quietly: the page still builds, the link just 404s. This runs over dist/ after a
// build and fails if any internal link has no target, if a root-relative asset (a cast, an image) is
// not in dist/, or if a .md link survived the rewrite. The site is served from the domain root, so
// every internal target is root-relative.

import { access, readdir, readFile } from "node:fs/promises";
import { join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL(".", import.meta.url));
const dist = join(here, "..", "dist");

async function walk(dir) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...(await walk(full)));
    else if (entry.name.endsWith(".html")) out.push(full);
  }
  return out;
}

const htmlFiles = await walk(dist);

// Every directory holding an index.html is a routable page.
const routes = new Set(["/"]);
for (const file of htmlFiles) {
  if (!file.endsWith(`${sep}index.html`)) continue;
  const dir = relative(dist, join(file, ".."));
  routes.add(dir === "" ? "/" : `/${dir.split(sep).join("/")}/`);
}

async function exists(path) {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

const failures = [];

for (const file of htmlFiles) {
  const html = await readFile(file, "utf8");
  const where = relative(dist, file);

  // data-src carries the cast path the player fetches at runtime, and poster the image a video
  // shows before it plays, so each is a link like any other.
  for (const [, attr, href] of html.matchAll(/\b(href|src|data-src|poster)="([^"]+)"/g)) {
    // A .md target means the rewrite missed it. GitHub links are intentional and fine.
    if (attr === "href" && href.includes(".md") && !href.includes("github.com")) {
      failures.push({ where, href, why: "unrewritten .md link" });
      continue;
    }
    // Root-relative only; "//host/..." is protocol-relative and external.
    if (!href.startsWith("/") || href.startsWith("//")) continue;

    const target = href.split(/[?#]/)[0];
    if (/\.[a-z0-9]{2,5}$/i.test(target)) {
      if (!(await exists(join(dist, decodeURIComponent(target))))) {
        failures.push({ where, href: target, why: "no such file" });
      }
      continue;
    }
    if (attr !== "href") continue;
    const normalized = target.endsWith("/") ? target : `${target}/`;
    if (!routes.has(normalized)) {
      failures.push({ where, href: normalized, why: "no such route" });
    }
  }
}

if (failures.length > 0) {
  const seen = new Set();
  console.error("broken internal links:");
  for (const f of failures) {
    const key = `${f.href}|${f.why}`;
    if (seen.has(key)) continue;
    seen.add(key);
    console.error(`  [${f.why}] ${f.href}  (in ${f.where})`);
  }
  process.exit(1);
}

console.log(`checked ${htmlFiles.length} pages, ${routes.size} routes — all internal links resolve`);
