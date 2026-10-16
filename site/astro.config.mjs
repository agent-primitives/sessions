// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import mermaid from "astro-mermaid";
import { readFileSync } from "node:fs";

// The docs under ../docs are the source of truth; hack/sync-docs.mjs copies them into
// src/content/docs before a build, so the site cannot drift from the repository.
export default defineConfig({
  // The dev toolbar never ships in a build, but it sits over the bottom of every page while
  // previewing, which gets in the way of judging the layout.
  devToolbar: { enabled: false },
  // Served from the root of a custom domain on the agent-primitives org's Pages, configured in the
  // repository's Pages settings with a DNS CNAME to agent-primitives.github.io. There is no base
  // path, so site routes and public/ assets are root-relative (/quickstart/, /casts/...).
  // Canonical links, og:url, and the sitemap are built from this address.
  site: "https://sessions.agent-primitives.dev",
  integrations: [
    // Must come before starlight so mermaid code fences are transformed before rendering.
    mermaid({
      theme: "neutral",
      autoTheme: true,
      mermaidConfig: {
        // Starlight's body font, so a diagram's labels match the prose around them. A var() rather
        // than a copied stack: mermaid sizes its boxes by measuring the text in the page, where the
        // variable resolves, so the boxes fit the font that is actually drawn.
        fontFamily: "var(--sl-font, var(--sl-font-system))",
        // Colours come from Starlight's tokens, which already differ per theme, so one stylesheet
        // serves both. Mermaid scopes it to each diagram and places it after its own theme rules.
        themeCSS: readFileSync(new URL("./src/styles/mermaid.css", import.meta.url), "utf8"),
      },
    }),
    starlight({
      title: "agentsessions",
      description:
        "A neutral contract for durable agent sessions: replay a run byte-for-byte without calling a model, fork it, and verify the record with a tool you wrote yourself.",
      social: [
        { icon: "github", label: "GitHub", href: "https://github.com/agent-primitives/sessions" },
      ],
      editLink: {
        baseUrl: "https://github.com/agent-primitives/sessions/edit/main/",
      },
      sidebar: [
        {
          label: "Start here",
          items: [
            { label: "Concepts", slug: "concepts" },
            { label: "How you use it", slug: "interaction-model" },
            { label: "Quickstart", slug: "quickstart" },
          ],
        },
        {
          label: "Build on it",
          items: [
            { label: "Writing a harness", slug: "harness-authoring" },
            { label: "Architecture", slug: "architecture" },
            { label: "Running on agent-substrate", slug: "substrate-conformance" },
          ],
        },
        {
          label: "Operate it",
          items: [
            { label: "Security posture", slug: "security" },
            { label: "Observability", slug: "observability" },
            { label: "Tool executors", slug: "tool-executors" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "API reference", slug: "api-reference" },
            { label: "Specification (draft)", slug: "spec" },
            { label: "FAQ", slug: "faq" },
          ],
        },
      ],
      components: {
        // Adds persistent nav links; splash pages have no sidebar, so without these the landing
        // page offers no route into the docs.
        SiteTitle: "./src/components/SiteTitle.astro",
      },
      // src/pages/404.astro replaces Starlight's 404 page with one that links back into the site.
      disable404Route: true,
      customCss: ["./src/styles/custom.css"],
      lastUpdated: true,
    }),
  ],
});
