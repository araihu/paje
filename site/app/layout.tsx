import type { Metadata, Viewport } from "next";
import { headers } from "next/headers";
import { Geist, Geist_Mono } from "next/font/google";
import {
  catalogs,
  localeFromRequestHeader,
  type Locale,
} from "./i18n/catalogs";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

async function metadataBase() {
  const incoming = await headers();
  const host = incoming.get("x-forwarded-host") ?? incoming.get("host") ?? "paje.araihu.com";
  const protocol = incoming.get("x-forwarded-proto") ?? (host.startsWith("localhost") ? "http" : "https");
  return new URL(`${protocol}://${host}`);
}

async function requestLocale(): Promise<Locale> {
  const incoming = await headers();
  return localeFromRequestHeader(incoming.get("x-paje-locale"));
}

export async function generateMetadata(): Promise<Metadata> {
  const locale = await requestLocale();
  const copy = catalogs[locale];
  const canonical = `/${locale}`;
  const alternateLocales = ["en_US", "pt_BR", "es_ES"].filter(
    (candidate) => candidate !== copy.metadata.openGraphLocale,
  );

  return {
    metadataBase: await metadataBase(),
    title: copy.metadata.title,
    description: copy.metadata.description,
    applicationName: "Pajé",
    icons: { icon: "/paje-icon-background.svg" },
    alternates: {
      canonical,
      languages: {
        en: "/en",
        "pt-BR": "/pt-br",
        es: "/es",
        "x-default": "/en",
      },
    },
    openGraph: {
      type: "website",
      url: canonical,
      locale: copy.metadata.openGraphLocale,
      alternateLocale: alternateLocales,
      siteName: "Pajé",
      title: copy.metadata.openGraphTitle,
      description: copy.metadata.openGraphDescription,
      images: [
        {
          url: "/og.png",
          width: 1731,
          height: 909,
          alt: copy.metadata.socialImageAlt,
        },
      ],
    },
    twitter: {
      card: "summary_large_image",
      title: copy.metadata.openGraphTitle,
      description: copy.metadata.openGraphDescription,
      images: ["/og.png"],
    },
  };
}

export const viewport: Viewport = {
  colorScheme: "light dark",
  themeColor: "#f2efe7",
};

export default async function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  const locale = await requestLocale();

  return (
    <html lang={catalogs[locale].htmlLang}>
      <body className={`${geistSans.variable} ${geistMono.variable}`}>{children}</body>
    </html>
  );
}
