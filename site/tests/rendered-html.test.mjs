import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const seasonalRuntimeURL = "https://araihu.com/assets/campaign/v1.js";
const seasonalChannelURL = "https://araihu.com/assets/releases/current";
const seasonalRuntimeSRI = "sha384-oPH7l1vK9vKP1Dn+18sO3yEXlz4ts6KzPEQl0SW4Y/+im05gOaamNNaQAf6bGH/n";
const pajeLogoFallback = "https://araihu.com/assets/releases/v0.1.1/brand/paje/logo/adaptive-plate-optical.svg";

const workerURL = new URL("../dist/server/index.js", import.meta.url);
workerURL.searchParams.set("test", `${process.pid}-${Date.now()}`);
const workerPromise = import(workerURL.href).then(({ default: worker }) => worker);

const runtime = {
  ASSETS: {
    async fetch(request) {
      const url = new URL(request.url);
      try {
        const body = await readFile(new URL(`../dist/client${url.pathname}`, import.meta.url));
        return new Response(body, { headers: { "content-type": url.pathname.endsWith(".html") ? "text/html" : "application/octet-stream" } });
      } catch {
        return new Response("Not found", { status: 404 });
      }
    },
  },
};

async function fetchSite(path, acceptLanguage) {
  const worker = await workerPromise;
  return worker.fetch(new Request(`https://paje.araihu.com${path}`, { headers: acceptLanguage ? { "accept-language": acceptLanguage } : {} }), runtime);
}

test("serves static localized documents", async () => {
  const expectations = {
    en: ["lang=\"en\"", "From request to pull request", "A run is a record", "Execution boundary", "Beta scope"],
    "pt-br": ["lang=\"pt-BR\"", "Do pedido ao pull request", "Um run é um registro", "Limite de execução", "Escopo do beta"],
    es: ["lang=\"es\"", "De la solicitud al pull request", "Una ejecución es un registro", "Límite de ejecución", "Alcance de la beta"],
  };
  for (const [locale, landmarks] of Object.entries(expectations)) {
    const response = await fetchSite(`/${locale}`, "fr");
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("content-language"), locale === "pt-br" ? "pt-BR" : locale);
    const html = await response.text();
    assert.match(html, /\/assets\/styles\.css/);
    assert.match(html, /<html[^>]*data-theme="araihu"[^>]*data-theme-source="default"/);
    assert.match(html, /\/araihu\.css/);
    assert.match(html, /<title>[^<]+ · Pajé<\/title>/);
    assert.match(html, new RegExp(`src="${pajeLogoFallback.replaceAll("/", "\\/")}"[^>]*width="166"[^>]*height="41"[^>]*data-asset-brand="logo"[^>]*crossorigin="anonymous"`));
    assert.match(html, /paje-icon-background\.svg/);
    assert.match(html, /<link rel="icon" href="\/paje-icon-background\.svg">/);
    assert.equal((html.match(/data-asset-brand="logo"/g) ?? []).length, 1);
    assert.equal((html.match(/data-campaign-toggle(?:\s|>)/g) ?? []).length, 1);
    assert.match(html, /<button[^>]*hidden[^>]*data-campaign-toggle[^>]*aria-pressed="false"/);
    assert.match(html, /data-campaign-toggle-icon/);

    const baselineScript = html.indexOf('src="/theme-toggle.js"');
    const runtimeScript = html.indexOf(`src="${seasonalRuntimeURL}"`);
    assert.ok(baselineScript > 0 && runtimeScript > baselineScript, "baseline theme code must precede campaign runtime");
    assert.match(html, new RegExp(`src="${seasonalRuntimeURL.replaceAll("/", "\\/")}"[^>]*data-channel="${seasonalChannelURL.replaceAll("/", "\\/")}"[^>]*defer[^>]*integrity="${seasonalRuntimeSRI.replaceAll("/", "\\/").replaceAll("+", "\\+")}"[^>]*crossorigin="anonymous"`));
    assert.doesNotMatch(html, /data-campaign=/);
    assert.equal((html.match(/rel="alternate" hreflang=/g) ?? []).length, 4);
    assert.match(html, new RegExp(`rel="canonical" href="https://paje\\.araihu\\.com/${locale}"`));
    assert.doesNotMatch(html, /alpine(?:js)?|htmx/i);
    assert.match(html, /\/assets\/js\/combobox\.js/);
    assert.doesNotMatch(html, /paje-(favicon|mark|mark-reverse)\.svg/);
    for (const landmark of landmarks) assert.match(html, new RegExp(landmark));
  }
});

test("keeps theme preference separate from campaign opt-out", async () => {
  const script = await readFile(new URL("../dist/client/theme-toggle.js", import.meta.url), "utf8");
  assert.match(script, /var key = "paje-theme"/);
  assert.match(script, /setAttribute\("data-theme-source", "preference"\)/);
  assert.match(script, /setAttribute\("data-color-scheme", theme\)/);
  assert.doesNotMatch(script, /data-campaign-toggle|campaign\.v1\.optout/);
});

test("keeps generator and Vinext enrollment sources aligned", async () => {
  const [layout, brand, page, toggle] = await Promise.all([
    readFile(new URL("../app/layout.tsx", import.meta.url), "utf8"),
    readFile(new URL("../app/brand.tsx", import.meta.url), "utf8"),
    readFile(new URL("../app/page.tsx", import.meta.url), "utf8"),
    readFile(new URL("../app/theme-toggle.tsx", import.meta.url), "utf8"),
  ]);
  for (const value of [seasonalRuntimeURL, seasonalChannelURL, seasonalRuntimeSRI]) assert.match(layout, new RegExp(value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  assert.match(layout, /data-theme-source="default"/);
  assert.match(brand, new RegExp(pajeLogoFallback.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  assert.match(brand, /data-asset-brand="logo"/);
  assert.equal((page.match(/<ManagedPajeLogo/g) ?? []).length, 1);
  assert.equal((page.match(/<CampaignToggle/g) ?? []).length, 1);
  assert.match(toggle, /data-theme-toggle/);
  assert.match(toggle, /data-campaign-toggle/);
});

test("keeps static navigation accessible across themes and viewports", async () => {
  const css = await readFile(new URL("../dist/client/site.css", import.meta.url), "utf8");
  const theme = await readFile(new URL("../dist/client/araihu.css", import.meta.url), "utf8");
  assert.match(theme, /\[data-theme="araihu"\]/);
  assert.match(theme, /--color-primary: #173b72/);
  assert.match(theme, /--color-primary-dark: #c7ff4a/);
  assert.match(css, /--surface:\s*var\(--color-surface\)/);
  assert.match(css, /--on-primary:\s*var\(--color-on-primary\)/);
  assert.match(css, /--surface:\s*var\(--color-surface-dark\)/);
  assert.match(css, /--run-muted:\s*#d5ddeb/);
  assert.match(css, /:root\.dark\s*\{[^}]*--surface:\s*var\(--color-surface-dark\)/s);
  assert.match(css, /:root:not\(\[data-color-scheme="light"\]\)/);
  assert.match(css, /\.languages a\s*\{[^}]*min-height:\s*44px/s);
  assert.match(css, /@media \(max-width:\s*760px\)[\s\S]*?\.hero h1\s*\{[^}]*font-size:\s*clamp\(2\.75rem, 12vw, 3\.5rem\)[^}]*overflow-wrap:\s*anywhere/s);
  assert.match(css, /@media \(max-width:\s*760px\)[\s\S]*?\.actions\s*\{[^}]*flex-direction:\s*column/s);
  assert.match(css, /a:focus-visible\s*\{/);
  assert.match(css, /prefers-reduced-motion:\s*no-preference/);
});

test("omits unused third-party runtime from the static artifact", async () => {
  for (const path of [
    "../dist/client/assets/js/runtime/alpinejs/3.14.9/alpine.min.js",
    "../dist/client/assets/js/runtime/htmx.org/2.0.8/htmx.min.js",
  ]) {
    await assert.rejects(readFile(new URL(path, import.meta.url)));
  }
  const combobox = await readFile(new URL("../dist/client/assets/js/combobox.js", import.meta.url), "utf8");
  assert.ok(combobox.length > 0);
});

test("packages the approved Pajé v11 brand assets", async () => {
  for (const name of [
    "paje-icon-background.svg",
    "paje-icon-transparent.svg",
    "paje-logo-background.svg",
    "paje-logo-transparent.svg",
  ]) {
    const contents = await readFile(new URL(`../dist/client/${name}`, import.meta.url), "utf8");
    assert.match(contents, /araihu-brand-v11/);
  }
});

test("negotiates root locale without serving dynamic HTML", async () => {
  for (const [header, path] of [["pt-BR", "/pt-br"], ["es-MX", "/es"], ["fr;q=1, es;q=.7", "/es"], ["@@@", "/en"]]) {
    const response = await fetchSite("/?source=docs", header);
    assert.equal(response.status, 307);
    assert.equal(new URL(response.headers.get("location")).pathname, path);
    assert.equal(response.headers.get("cache-control"), "private, no-store");
  }
});
