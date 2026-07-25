import type { Metadata, Viewport } from "next";
import { headers } from "next/headers";
import { Geist, Geist_Mono } from "next/font/google";
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

export async function generateMetadata(): Promise<Metadata> {
  return {
    metadataBase: await metadataBase(),
    title: "Pajé — Orquestração durável para agentes de código",
    description:
      "Do pedido ao pull request com memória, execução isolada, artefatos verificáveis, aprovação humana e publicação idempotente.",
    applicationName: "Pajé",
    alternates: { canonical: "/" },
    openGraph: {
      type: "website",
      locale: "pt_BR",
      siteName: "Pajé",
      title: "Pajé — Do pedido ao pull request. Sem perder o fio.",
      description: "Orquestração durável e self-hosted para agentes de código.",
      images: [
        {
          url: "/og.png",
          width: 1731,
          height: 909,
          alt: "Pajé — Do pedido ao pull request. Sem perder o fio.",
        },
      ],
    },
    twitter: {
      card: "summary_large_image",
      title: "Pajé — Do pedido ao pull request. Sem perder o fio.",
      description: "Orquestração durável e self-hosted para agentes de código.",
      images: ["/og.png"],
    },
  };
}

export const viewport: Viewport = {
  colorScheme: "light",
  themeColor: "#f2efe7",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="pt-BR">
      <body className={`${geistSans.variable} ${geistMono.variable}`}>{children}</body>
    </html>
  );
}
