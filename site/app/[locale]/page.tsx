import { notFound } from "next/navigation";
import { isLocale } from "../i18n/catalogs";
import { LocalizedHome } from "../page";

interface LocalizedPageProps {
  params: Promise<{ locale: string }>;
}

export default async function LocalizedPage({ params }: LocalizedPageProps) {
  const { locale } = await params;

  if (!isLocale(locale)) {
    notFound();
  }

  return <LocalizedHome locale={locale} />;
}
