import assert from "node:assert/strict";
import test from "node:test";

async function render() {
  const workerUrl = new URL("../dist/server/index.js", import.meta.url);
  workerUrl.searchParams.set("test", `${process.pid}-${Date.now()}`);
  const { default: worker } = await import(workerUrl.href);

  return worker.fetch(
    new Request("https://paje.araihu.com/", { headers: { accept: "text/html" } }),
    { ASSETS: { fetch: async () => new Response("Not found", { status: 404 }) } },
    { waitUntil() {}, passThroughOnException() {} },
  );
}

test("server-renders the Pajé product site", async () => {
  const response = await render();
  assert.equal(response.status, 200);
  assert.match(response.headers.get("content-type") ?? "", /^text\/html\b/i);

  const html = await response.text();
  assert.match(html, /<html[^>]*lang="pt-BR"/i);
  assert.match(html, /<title>Pajé — Orquestração durável pilotada pelo agente<\/title>/i);
  assert.match(html, /Do pedido ao pull request/);
  assert.match(html, /code-change@v1/);
  assert.match(html, /pilotado pelo próprio agente via hooks e skills/i);
  assert.match(html, /Hooks e skills são a superfície de integração pretendida/i);
  assert.match(html, /Hoje, inicie.*paje-code-change-v1.*no Hatchet/is);
  assert.match(html, /independente da linguagem/i);
  assert.match(html, /toolchain correspondente na imagem do worker/i);
  assert.match(html, /Codex é o primeiro harness/i);
  assert.match(html, /Outros harnesses serão suportados no futuro/i);
  assert.match(html, /profile: generic/i);
  assert.match(html, /Guia rápido/);
  assert.match(html, /https:\/\/github\.com\/araihu\/paje/);
  assert.match(html, /https:\/\/paje\.araihu\.com\/og\.png/);
  assert.doesNotMatch(html, /Go-native|Go de verdade|codex-preview|react-loading-skeleton|Your site is taking shape/i);
});
