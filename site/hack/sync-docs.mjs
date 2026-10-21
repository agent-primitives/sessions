// Copies the repository docs into the Starlight content directory.
//
// The files under ../../docs are the source of truth. Running this before a build means the site
// cannot drift from them, and it keeps the docs readable on GitHub, where most people will meet
// them first. Everything it writes is generated, so src/content/docs is gitignored.
//
// Two transformations are needed. Starlight requires frontmatter, which the plain markdown does not
// carry, and it serves pages at extensionless routes, so `concepts.md` has to become `/concepts/`.
//
// Because the published copy is generated and gitignored, Starlight cannot see where a page came
// from: its edit link would point at the copy (a 404 on GitHub) and git has no history for it. So
// the frontmatter also carries the edit URL of the source (under docs/, or spec.md at the root) and
// its last commit date.
// A doc that is itself generated (api-reference.md, from the proto comments) has no single file to
// edit, so it gets no edit link rather than one to a file whose banner says not to edit it.
import { execFileSync } from "node:child_process";
import { copyFile, mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const docsDir = join(here, "..", "..", "docs");
const outDir = join(here, "..", "src", "content", "docs");
const repoRoot = join(here, "..", "..");
const repo = "https://github.com/agent-primitives/sessions";

// README.md is the docs index; the Starlight sidebar replaces it, so it is not published.
const skip = new Set(["README.md"]);

// Descriptions are written here rather than derived from the first paragraph, which is usually a
// sentence of context rather than a summary and reads badly as a page description.
const meta = {
  "substrate-kind.md": {
    title: "Durable kind example",
    description:
      "Deploy the existing Substrate daemon with isolated chat harnesses and a journal that survives pod replacement.",
  },
  "api-reference.md": {
    title: "API reference",
    description:
      "Every message, field, enum, and RPC in agentsessions.v1, generated from the proto comments.",
  },
  "architecture.md": {
    title: "Architecture",
    description:
      "How the neutral core is built: three contracts, a durable hash-chained log, and the determinism invariants the controller enforces.",
  },
  "concepts.md": {
    title: "Concepts",
    description:
      "The nouns: sessions, typed events, the log, incarnations, fences, resumability, and capabilities.",
  },
  "faq.md": {
    title: "FAQ",
    description: "What agentsessions is, what it is not, and how it behaves.",
  },
  "harness-authoring.md": {
    title: "Writing a harness",
    description:
      "Plug your agent into the api.Harness SPI: two methods, and the rules that keep replay exact.",
  },
  "interaction-model.md": {
    title: "How you use it",
    description:
      "What you implement, what you call, and what the project deliberately does not provide.",
  },
  "observability.md": {
    title: "Observability",
    description: "Structured operation logs, request correlation, and the data-safety contract.",
  },
  "quickstart.md": {
    title: "Quickstart",
    description:
      "Exec a turn, replay it with zero model calls, fork it, suspend and resume it, and verify the provenance chain.",
  },
  "responses-adapter.md": {
    title: "Responses model adapter",
    description:
      "Explicit stateless Responses calls with function tools, opaque reasoning continuity, usage, and text streaming.",
  },
  "security.md": {
    title: "Security posture",
    description:
      "What is protected, what is not, and how to deploy it accordingly. Read before exposing a host.",
  },
  "approval-gated-tool.md": {
    title: "Approval-gated tool example",
    description:
      "A model calls a tool on the stock daemon; the session parks, survives a restart, and runs the call once after approval.",
  },
  "tool-executors.md": {
    title: "Tool executors",
    description:
      "Configure exact harness/tool routes to operator-run Unix sockets, with approval gates and safe recovery.",
  },
  "substrate-conformance.md": {
    title: "Running on agent-substrate",
    description:
      "Both capability tiers on a real cluster, on a micro-VM: stateless replay and memory-snapshot continuity.",
  },
};

// Documents published from the repository root rather than docs/. The specification lives at the
// root, where implementers look for it, so its links are relative to the root, not to docs/.
const rootDocs = {
  "spec.md": {
    title: "Specification (draft)",
    description:
      "The normative rules of the AgentSessions protocol: session model, event log, provenance chain, harness wire protocol, runtime contract, and conformance classes.",
  },
};

/** Rewrite links so they resolve on the site instead of on GitHub. */
function rewriteLinks(markdown) {
  return (
    markdown
      // A sibling doc becomes a site route: (concepts.md#anchor) -> (/concepts/#anchor).
      // In the repository the link text is usually the bare filename, which reads as a broken
      // artifact on a rendered site, so swap it for the page title we already have.
      .replace(
        /\[([^\]]+)\]\((?!https?:|\/)([a-z0-9-]+)\.md(#[^)]*)?\)/g,
        (_m, text, name, anchor) => {
          const isFilename = text.replace(/`/g, "") === `${name}.md`;
          const label = isFilename ? (meta[`${name}.md`]?.title ?? text) : text;
          return `[${label}](/${name}/${anchor ?? ""})`;
        },
      )
      // Anything above docs/ only exists in the repository, so point at GitHub.
      .replace(/\]\(\.\.\/([^)]+)\)/g, (_m, path) => `](${repo}/blob/main/${path})`)
  );
}

/** Rewrite a root document's links: docs/ pages become site routes, everything else GitHub. */
function rewriteRootLinks(markdown) {
  return (
    markdown
      // (docs/concepts.md#anchor) -> (/concepts/#anchor), with a bare filename label swapped for
      // the page title, as rewriteLinks does for docs/.
      .replace(
        /\[([^\]]+)\]\(docs\/([a-z0-9-]+)\.md(#[^)]*)?\)/g,
        (_m, text, name, anchor) => {
          const isFilename = text.replace(/`/g, "") === `docs/${name}.md`;
          const label = isFilename ? (meta[`${name}.md`]?.title ?? text) : text;
          return `[${label}](/${name}/${anchor ?? ""})`;
        },
      )
      // Any other relative path only exists in the repository, so point at GitHub.
      .replace(/\]\((?!https?:|\/|#)([^)]+)\)/g, (_m, path) => `](${repo}/blob/main/${path})`)
  );
}

/**
 * The specification marks unresolved questions `TODO(spike): <question>` in the repository, where
 * they are the review checklist. Published, they are open questions of a draft, so they render as
 * such. Only this exact form is rewritten; any other marker still fails the check below.
 */
function renderOpenQuestions(markdown) {
  return markdown.replace(/\bTODO\(spike\):\s*/g, "Open question: ");
}

/** Strip the leading H1; Starlight renders the title from frontmatter. */
function stripTitle(markdown) {
  return markdown.replace(/^#\s+.+\n+/, "");
}

/** Strip the generated-file banner, which is noise on a rendered page. */
function stripBanner(markdown) {
  return markdown.replace(/^<!--[\s\S]*?-->\n+/, "");
}

/** True if the doc opens with the generated-file banner (see hack/gen-api-reference.sh). */
function isGenerated(markdown) {
  return /^<!--\s*GENERATED FILE\. DO NOT EDIT\./.test(markdown);
}

/** Runs git in the repository root; null if git or the repository is unavailable. */
function git(...args) {
  try {
    return execFileSync("git", args, {
      cwd: repoRoot,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
  } catch {
    return null;
  }
}

// A shallow clone (CI's default checkout) would date every page with the one commit it has, which
// is wrong rather than missing, so dates are left out there. site.yml fetches full history.
const shallow = git("rev-parse", "--is-shallow-repository");
const haveHistory = shallow === "false";
if (!haveHistory) {
  console.warn(
    "sync-docs: no full git history (shallow clone or no git), so pages carry no last-updated date",
  );
}

/** The last commit that touched `path` (repository-relative), as an ISO 8601 timestamp, or null. */
function lastUpdated(path) {
  if (!haveHistory) return null;
  return git("log", "-1", "--format=%cI", "--", path) || null;
}

function yamlQuote(value) {
  return `"${value.replace(/"/g, '\\"')}"`;
}

/**
 * Frontmatter for a page synced from `path` (repository-relative, e.g. docs/concepts.md or
 * spec.md), whose raw content is `source`: the edit URL and last-updated date of that file, not of
 * the gitignored copy (see the header).
 */
function frontmatterFor(info, path, source) {
  const updated = lastUpdated(path);
  return [
    "---",
    `title: ${yamlQuote(info.title)}`,
    `description: ${yamlQuote(info.description)}`,
    `editUrl: ${isGenerated(source) ? "false" : yamlQuote(`${repo}/edit/main/${path}`)}`,
    // Unquoted, so YAML reads it as a timestamp, which is what Starlight's schema expects.
    ...(updated ? [`lastUpdated: ${updated}`] : []),
    "---",
    "",
  ].join("\n");
}

// Markers that are fine in a working repository but must never reach a published page: they read
// as leaked engineering notes to anyone who did not write them. Failing the sync is deliberate, so
// the choice is made in the doc rather than silently shipped.
const internalMarkers = /^.*\b(TODO|FIXME|XXX|HACK)\b(\(|:).*$/gm;

function assertNoInternalMarkers(label, markdown) {
  const hits = markdown.match(internalMarkers);
  if (!hits) return;
  throw new Error(
    `${label} contains internal markers that would be published:\n` +
      hits.map((h) => `    ${h.trim()}`).join("\n") +
      `\n  Resolve the note, move it to an issue, or rewrite it as prose that a reader can use.`,
  );
}

const files = (await readdir(docsDir)).filter((f) => f.endsWith(".md") && !skip.has(f));

await rm(outDir, { recursive: true, force: true });
await mkdir(outDir, { recursive: true });

for (const file of files) {
  const info = meta[file];
  if (!info) {
    throw new Error(
      `docs/${file} has no entry in hack/sync-docs.mjs. Add a title and description, ` +
        `or add it to the skip list, so a new doc cannot be published untitled.`,
    );
  }

  const source = await readFile(join(docsDir, file), "utf8");
  assertNoInternalMarkers(`docs/${file}`, source);
  const body = rewriteLinks(stripTitle(stripBanner(source)));
  await writeFile(join(outDir, file), frontmatterFor(info, `docs/${file}`, source) + body);
}

for (const [file, info] of Object.entries(rootDocs)) {
  const source = renderOpenQuestions(await readFile(join(repoRoot, file), "utf8"));
  assertNoInternalMarkers(file, source);
  const body = rewriteRootLinks(stripTitle(source));
  await writeFile(join(outDir, file), frontmatterFor(info, file, source) + body);
}

// The landing page is hand-written rather than synced from a doc, so it lives outside the generated
// directory and is copied in last. Everything under src/content/docs is generated, which is what
// makes wiping it safe. It is copied rather than rewritten, but it still has to clear the marker
// check: it is the most-read page on the site, and it was the one page the guard did not cover.
const landing = join(here, "..", "src", "landing", "index.mdx");
assertNoInternalMarkers("site/src/landing/index.mdx", await readFile(landing, "utf8"));
await copyFile(landing, join(outDir, "index.mdx"));

console.log(
  `synced ${files.length} docs, ${Object.keys(rootDocs).length} root docs, and the landing page into src/content/docs`,
);
