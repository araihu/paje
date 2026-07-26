import assert from "node:assert/strict";
import test from "node:test";

const workerUrl = new URL("../dist/server/index.js", import.meta.url);
workerUrl.searchParams.set("test", `${process.pid}-${Date.now()}`);
const workerPromise = import(workerUrl.href).then(({ default: worker }) => worker);

const runtime = {
  ASSETS: { fetch: async () => new Response("Not found", { status: 404 }) },
};
const executionContext = {
  waitUntil() {},
  passThroughOnException() {},
};

async function fetchSite(path, acceptLanguage, additionalHeaders = {}) {
  const worker = await workerPromise;
  const headers = { accept: "text/html", ...additionalHeaders };
  if (acceptLanguage !== undefined) {
    headers["accept-language"] = acceptLanguage;
  }

  return worker.fetch(
    new Request(`https://paje.araihu.com${path}`, { headers }),
    runtime,
    executionContext,
  );
}

async function render(locale, acceptLanguage) {
  const response = await fetchSite(`/${locale}`, acceptLanguage);
  assert.equal(response.status, 200);
  assert.match(response.headers.get("content-type") ?? "", /^text\/html\b/i);
  return { response, html: await response.text() };
}

function escapePattern(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function assertMeta(html, attribute, name, content) {
  const tag = new RegExp(
    `<meta(?=[^>]*${attribute}="${escapePattern(name)}")(?=[^>]*content="${escapePattern(content)}")[^>]*>`,
    "i",
  );
  assert.match(html, tag);
}

function assertLink(html, rel, attribute, value, href) {
  const tag = new RegExp(
    `<link(?=[^>]*rel="${escapePattern(rel)}")(?=[^>]*${attribute}="${escapePattern(value)}")(?=[^>]*href="${escapePattern(href)}")[^>]*>`,
    "i",
  );
  assert.match(html, tag);
}

test("negotiates the root locale with q-values, wildcards, and policy fallbacks", async (t) => {
  const cases = [
    ["pt-BR", "/pt-br"],
    ["pt-PT", "/pt-br"],
    ["pt", "/pt-br"],
    ["es", "/es"],
    ["es-ES", "/es"],
    ["es-MX", "/es"],
    ["fr;q=1, es-MX;q=0.8", "/es"],
    ["pt-BR;q=0.4, es;q=0.9", "/es"],
    ["pt-BR;q=0, es;q=0.5", "/es"],
    ["en", "/en"],
    ["fr", "/en"],
    ["de", "/en"],
    ["pt-AO", "/en"],
    ["pt-US", "/en"],
    ["es;q=0.4, en;q=0.8", "/en"],
    ["fr;q=0.9, *;q=0.8, es;q=0.7", "/en"],
    ["pt-AO;q=1, *;q=.8", "/en"],
    ["@@@;q=banana", "/en"],
    [undefined, "/en"],
  ];

  for (const [header, expectedPath] of cases) {
    await t.test(header ?? "missing header", async () => {
      const response = await fetchSite("/", header);
      assert.equal(response.status, 307);
      assert.equal(new URL(response.headers.get("location")).pathname, expectedPath);
      assert.equal(response.headers.get("cache-control"), "private, no-store");
      assert.equal(response.headers.get("vary"), "Accept-Language");
    });
  }
});

test("root negotiation preserves the query string without creating a redirect loop", async () => {
  const response = await fetchSite("/?campaign=agent-piloted&source=docs", "es-MX");
  assert.equal(response.status, 307);
  assert.equal(
    response.headers.get("location"),
    "https://paje.araihu.com/es?campaign=agent-piloted&source=docs",
  );

  const explicitResponse = await fetchSite("/es?campaign=agent-piloted&source=docs", "pt-BR");
  assert.equal(explicitResponse.status, 200);
  assert.equal(explicitResponse.headers.get("content-language"), "es");
});

test("explicit locale routes override Accept-Language", async () => {
  const englishResponse = await fetchSite(
    "/en",
    "pt-BR,es;q=0.8",
    { "x-paje-locale": "pt-br" },
  );
  assert.equal(englishResponse.status, 200);
  assert.equal(englishResponse.headers.get("content-language"), "en");
  assert.match(await englishResponse.text(), /From request to pull request/);

  const portuguese = await render("pt-br", "es-MX,en;q=0.8");
  assert.match(portuguese.html, /Do pedido ao pull request/);
  assert.equal(portuguese.response.headers.get("content-language"), "pt-BR");

  const spanish = await render("es", "en,pt-BR;q=0.8");
  assert.match(spanish.html, /De la solicitud al pull request/);
  assert.equal(spanish.response.headers.get("content-language"), "es");
});

const localeExpectations = {
  en: {
    htmlLang: "en",
    title: "Pajé — Durable orchestration piloted by the agent",
    description:
      "Designed for the agent to pilot through hooks and skills. Pajé makes changes durable for repositories in any language, with Codex as the first harness.",
    ogTitle: "Pajé — From request to pull request. Without losing the thread.",
    ogDescription:
      "Designed for the agent to pilot, repository-language-neutral, with Codex as the first harness.",
    ogLocale: "en_US",
    landmarks: [
      /From request to pull request/,
      /Designed for the agent to pilot/,
      /Workflows are repository-language-neutral/,
      /Go-native positioning is inconsequential/,
      /Codex is the first supported harness/,
      /other harnesses in the future/,
      /Quick guide/,
      /Security through boundaries/,
      /Documentation/,
      /Increase the client timeout and update the tests/,
    ],
    forbidden: [
      /Do pedido ao pull request/,
      /De la solicitud al pull request/,
      /Guia rápido/,
      /Segurança por fronteiras/,
    ],
  },
  "pt-br": {
    htmlLang: "pt-BR",
    title: "Pajé — Orquestração durável pilotada pelo agente",
    description:
      "Projetado para o agente pilotar via hooks e skills. Pajé torna mudanças duráveis em qualquer linguagem, com Codex como primeiro harness.",
    ogTitle: "Pajé — Do pedido ao pull request. Sem perder o fio.",
    ogDescription:
      "Projetado para o agente pilotar, independente da linguagem e com Codex como primeiro harness.",
    ogLocale: "pt_BR",
    landmarks: [
      /Do pedido ao pull request/,
      /pilotado pelo próprio agente via hooks e skills/i,
      /Hooks e skills são a superfície de integração pretendida/i,
      /Hoje, inicie.*paje-code-change-v1.*no Hatchet/is,
      /independente da linguagem/i,
      /toolchain correspondente na imagem do worker/i,
      /Codex é o primeiro harness/i,
      /Outros harnesses serão suportados no futuro/i,
      /Guia rápido/,
      /Segurança por fronteiras/,
      /Documentação/,
      /Aumente o timeout do cliente e atualize os testes/,
    ],
    forbidden: [
      /From request to pull request/,
      /De la solicitud al pull request/,
      /Quick guide/,
      /Seguridad mediante fronteras/,
    ],
  },
  es: {
    htmlLang: "es",
    title: "Pajé — Orquestación duradera pilotada por el agente",
    description:
      "Diseñado para que el agente lo pilote mediante hooks y skills. Pajé hace duraderos los cambios en repositorios de cualquier lenguaje, con Codex como primer harness.",
    ogTitle: "Pajé — De la solicitud al pull request. Sin perder el hilo.",
    ogDescription:
      "Diseñado para que lo pilote el agente, neutral respecto al lenguaje del repositorio y con Codex como primer harness.",
    ogLocale: "es_ES",
    landmarks: [
      /De la solicitud al pull request/,
      /propio agente lo pilote mediante hooks y skills/i,
      /Los workflows son neutrales respecto al lenguaje del repositorio/i,
      /posicionarlo como Go-native es irrelevante/i,
      /Codex es el primer harness soportado/i,
      /otros harnesses en el futuro/i,
      /Guía rápida/,
      /Seguridad mediante fronteras/,
      /Documentación/,
      /Aumenta el timeout del cliente y actualiza las pruebas/,
    ],
    forbidden: [
      /Do pedido ao pull request/,
      /From request to pull request/,
      /Guia rápido/,
      /Security through boundaries/,
    ],
  },
};

for (const [locale, expected] of Object.entries(localeExpectations)) {
  test(`renders a complete ${locale} catalog with localized SEO`, async () => {
    const { html } = await render(locale);
    assert.match(html, new RegExp(`<html[^>]*lang="${expected.htmlLang}"`, "i"));
    assert.match(html, new RegExp(`<title>${escapePattern(expected.title)}</title>`, "i"));
    assertMeta(html, "name", "description", expected.description);
    assertMeta(html, "property", "og:title", expected.ogTitle);
    assertMeta(html, "property", "og:description", expected.ogDescription);
    assertMeta(html, "property", "og:locale", expected.ogLocale);
    assertMeta(html, "name", "twitter:title", expected.ogTitle);
    assertMeta(html, "name", "twitter:description", expected.ogDescription);
    assertLink(html, "canonical", "href", `https://paje.araihu.com/${locale}`, `https://paje.araihu.com/${locale}`);
    assert.match(html, /https:\/\/paje\.araihu\.com\/og\.png/);

    for (const landmark of expected.landmarks) {
      assert.match(html, landmark);
    }
    for (const forbidden of expected.forbidden) {
      assert.doesNotMatch(html, forbidden);
    }

    assert.doesNotMatch(html, /MISSING_TRANSLATION|>undefined<|>null</i);
    assert.doesNotMatch(html, /Go-native product|Go-native orchestration|codex-preview|react-loading-skeleton|Your site is taking shape/i);
  });
}

test("emits canonical hreflang alternates and an accessible selected language switcher", async () => {
  for (const locale of ["en", "pt-br", "es"]) {
    const { html } = await render(locale);

    assertLink(html, "alternate", "hrefLang", "en", "https://paje.araihu.com/en");
    assertLink(html, "alternate", "hrefLang", "pt-BR", "https://paje.araihu.com/pt-br");
    assertLink(html, "alternate", "hrefLang", "es", "https://paje.araihu.com/es");
    assertLink(html, "alternate", "hrefLang", "x-default", "https://paje.araihu.com/en");

    assert.match(html, /<nav[^>]*class="language-switcher"[^>]*aria-label="[^"]+"/i);
    assert.match(html, /href="\/en"[^>]*hreflang="en"/i);
    assert.match(html, /href="\/pt-br"[^>]*hreflang="pt-BR"/i);
    assert.match(html, /href="\/es"[^>]*hreflang="es"/i);

    const selected = new RegExp(
      `<a(?=[^>]*aria-current="page")(?=[^>]*href="/${escapePattern(locale)}")[^>]*>`,
      "i",
    );
    assert.match(html, selected);
    assert.equal((html.match(/aria-current="page"/g) ?? []).length, 1);
  }
});

test("language controls retain explicit routes and implement query and anchor preservation", async () => {
  const { html } = await render("pt-br");
  assert.match(html, /href="\/en"/);
  assert.match(html, /href="\/pt-br"/);
  assert.match(html, /href="\/es"/);

  const switcherSource = await import("node:fs/promises").then(({ readFile }) =>
    readFile(new URL("../app/i18n/language-switcher.tsx", import.meta.url), "utf8"),
  );
  assert.match(switcherSource, /destination\.search = window\.location\.search/);
  assert.match(switcherSource, /destination\.hash = window\.location\.hash/);
});
