export type Locale = "en" | "pt-br" | "es";

interface Env {
  ASSETS: { fetch(request: Request): Promise<Response> };
}

interface LanguagePreference {
  range: string;
  quality: number;
  order: number;
}

const languages: Record<Locale, string> = { en: "en", "pt-br": "pt-BR", es: "es" };
const localeRoute = /^\/(en|pt-br|es)\/?$/;
const languagePreference = /^([A-Za-z0-9*](?:[A-Za-z0-9-]*))(?:\s*;\s*q\s*=\s*(0(?:\.\d{0,3})?|\.\d{1,3}|1(?:\.0{0,3})?))?$/i;

function parsePreference(part: string, order: number): LanguagePreference | null {
  const match = languagePreference.exec(part.trim());
  if (!match) return null;
  const quality = match[2] === undefined ? 1 : Number(match[2]);
  if (!Number.isFinite(quality) || quality < 0 || quality > 1) return null;
  if (match[1] === "*") return { range: "*", quality, order };
  try {
    return { range: Intl.getCanonicalLocales(match[1])[0], quality, order };
  } catch {
    return null;
  }
}

function localeForRange(range: string): Locale | null {
  const normalized = range.toLowerCase();
  if (normalized === "*" || normalized === "en" || normalized.startsWith("en-")) return "en";
  if (normalized === "pt" || normalized === "pt-br" || normalized === "pt-pt") return "pt-br";
  if (normalized === "es" || normalized.startsWith("es-")) return "es";
  return null;
}

export function negotiateLocale(header: string | null): Locale {
  if (!header?.trim()) return "en";
  const preferences = header.split(",").map(parsePreference);
  if (preferences.some((item) => item === null)) return "en";
  for (const preference of (preferences as LanguagePreference[])
    .filter((item) => item.quality > 0)
    .sort((a, b) => b.quality - a.quality || a.order - b.order)) {
    const locale = localeForRange(preference.range);
    if (locale) return locale;
  }
  return "en";
}

function withLocale(response: Response, locale: Locale): Response {
  const headers = new Headers(response.headers);
  headers.set("content-language", languages[locale]);
  headers.set("cache-control", "public, max-age=300");
  return new Response(response.body, { status: response.status, statusText: response.statusText, headers });
}

const worker = {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/" && (request.method === "GET" || request.method === "HEAD")) {
      const destination = new URL(request.url);
      destination.pathname = `/${negotiateLocale(request.headers.get("accept-language"))}`;
      return new Response(null, { status: 307, headers: { location: destination.toString(), vary: "Accept-Language", "cache-control": "private, no-store" } });
    }
    const match = localeRoute.exec(url.pathname);
    if (match && (request.method === "GET" || request.method === "HEAD")) {
      const locale = match[1] as Locale;
      const assetURL = new URL(request.url);
      assetURL.pathname = `/${locale}/index.html`;
      return withLocale(await env.ASSETS.fetch(new Request(assetURL, request)), locale);
    }
    return env.ASSETS.fetch(request);
  },
};

export default worker;
