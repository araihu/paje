import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

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
    en: ["lang=\"en\"", "From request to pull request", "A run is a record"],
    "pt-br": ["lang=\"pt-BR\"", "Do pedido ao pull request", "Um run é um registro"],
    es: ["lang=\"es\"", "De la solicitud al pull request", "Una ejecución es un registro"],
  };
  for (const [locale, landmarks] of Object.entries(expectations)) {
    const response = await fetchSite(`/${locale}`, "fr");
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("content-language"), locale === "pt-br" ? "pt-BR" : locale);
    const html = await response.text();
    assert.match(html, /\/assets\/styles\.css/);
    assert.match(html, /paje-mark\.svg/);
    for (const landmark of landmarks) assert.match(html, new RegExp(landmark));
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
