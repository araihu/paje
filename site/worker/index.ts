/** Cloudflare Worker entry point for the vinext-starter template. */
import { handleImageOptimization, DEFAULT_DEVICE_SIZES, DEFAULT_IMAGE_SIZES } from "vinext/server/image-optimization";
import handler from "vinext/server/app-router-entry";
import { catalogs, type Locale } from "../app/i18n/catalogs";

interface Env {
  ASSETS: {
    fetch(request: Request): Promise<Response>;
  };
  IMAGES: {
    input(stream: ReadableStream): {
      transform(options: Record<string, unknown>): {
        output(options: { format: string; quality: number }): Promise<{ response(): Response }>;
      };
    };
  };
}

interface ExecutionContext {
  waitUntil(promise: Promise<unknown>): void;
  passThroughOnException(): void;
}

interface LanguagePreference {
  range: string;
  quality: number;
  order: number;
}

const localeRoutePattern = /^\/(en|pt-br|es)\/?$/;
const languagePreferencePattern =
  /^([A-Za-z0-9*](?:[A-Za-z0-9-]*))(?:\s*;\s*q\s*=\s*(0(?:\.\d{0,3})?|\.\d{1,3}|1(?:\.0{0,3})?))?$/i;

function parseLanguagePreference(part: string, order: number): LanguagePreference | null {
  const match = languagePreferencePattern.exec(part.trim());
  if (!match) {
    return null;
  }

  const [, rawRange, rawQuality] = match;
  const quality = rawQuality === undefined ? 1 : Number(rawQuality);
  if (!Number.isFinite(quality) || quality < 0 || quality > 1) {
    return null;
  }

  if (rawRange === "*") {
    return { range: rawRange, quality, order };
  }

  try {
    const [canonicalRange] = Intl.getCanonicalLocales(rawRange);
    return { range: canonicalRange, quality, order };
  } catch {
    return null;
  }
}

function localeForRange(range: string): Locale | null {
  if (range === "*") {
    return "en";
  }

  const normalized = range.toLowerCase();
  if (normalized === "pt" || normalized === "pt-br" || normalized === "pt-pt") {
    return "pt-br";
  }
  if (normalized === "es" || normalized.startsWith("es-")) {
    return "es";
  }
  if (normalized === "en" || normalized.startsWith("en-")) {
    return "en";
  }

  return null;
}

export function negotiateLocale(header: string | null): Locale {
  if (header === null || header.trim() === "") {
    return "en";
  }

  const parts = header.split(",");
  if (parts.some((part) => part.trim() === "")) {
    return "en";
  }

  const preferences = parts.map(parseLanguagePreference);
  if (preferences.some((preference) => preference === null)) {
    return "en";
  }

  const ordered = (preferences as LanguagePreference[])
    .filter((preference) => preference.quality > 0)
    .sort((left, right) => right.quality - left.quality || left.order - right.order);

  for (const preference of ordered) {
    const locale = localeForRange(preference.range);
    if (locale) {
      return locale;
    }
  }

  return "en";
}

function localeRedirect(request: Request): Response {
  const locale = negotiateLocale(request.headers.get("accept-language"));
  const destination = new URL(request.url);
  destination.pathname = `/${locale}`;

  return new Response(null, {
    status: 307,
    headers: {
      "cache-control": "private, no-store",
      "content-language": catalogs[locale].htmlLang,
      location: destination.toString(),
      vary: "Accept-Language",
    },
  });
}

function requestWithLocale(request: Request, locale: Locale): Request {
  const headers = new Headers(request.headers);
  headers.set("x-paje-locale", locale);
  return new Request(request, { headers });
}

function responseWithLocale(response: Response, locale: Locale): Response {
  const headers = new Headers(response.headers);
  headers.set("content-language", catalogs[locale].htmlLang);
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

// Image security config. SVG sources with .svg extension auto-skip the
// optimization endpoint on the client side (served directly, no proxy).
// To route SVGs through the optimizer (with security headers), set
// dangerouslyAllowSVG: true in next.config.js and uncomment below:
// const imageConfig: ImageConfig = { dangerouslyAllowSVG: true };

const worker = {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(request.url);

    if (
      url.pathname === "/" &&
      (request.method === "GET" || request.method === "HEAD")
    ) {
      return localeRedirect(request);
    }

    if (url.pathname === "/_vinext/image") {
      const allowedWidths = [...DEFAULT_DEVICE_SIZES, ...DEFAULT_IMAGE_SIZES];
      return handleImageOptimization(request, {
        fetchAsset: (path) => env.ASSETS.fetch(new Request(new URL(path, request.url))),
        transformImage: async (body, { width, format, quality }) => {
          const result = await env.IMAGES.input(body).transform(width > 0 ? { width } : {}).output({ format, quality });
          return result.response();
        },
      }, allowedWidths);
    }

    const localeMatch = localeRoutePattern.exec(url.pathname);
    if (localeMatch) {
      const locale = localeMatch[1] as Locale;
      const response = await handler.fetch(requestWithLocale(request, locale), env, ctx);
      return responseWithLocale(response, locale);
    }

    return handler.fetch(requestWithLocale(request, "en"), env, ctx);
  },
};

export default worker;
