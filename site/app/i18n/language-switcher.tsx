"use client";

import type { MouseEvent } from "react";
import type { Locale } from "./catalogs";

const options: readonly { locale: Locale; label: string; hrefLang: string }[] = [
  { locale: "en", label: "EN", hrefLang: "en" },
  { locale: "pt-br", label: "PT-BR", hrefLang: "pt-BR" },
  { locale: "es", label: "ES", hrefLang: "es" },
];

interface LanguageSwitcherProps {
  currentLocale: Locale;
  label: string;
  languageLabels: Record<Locale, string>;
}

export function LanguageSwitcher({
  currentLocale,
  label,
  languageLabels,
}: LanguageSwitcherProps) {
  function preserveLocation(event: MouseEvent<HTMLAnchorElement>) {
    if (
      event.defaultPrevented ||
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return;
    }

    event.preventDefault();
    const destination = new URL(event.currentTarget.href);
    destination.search = window.location.search;
    destination.hash = window.location.hash;
    window.location.assign(destination);
  }

  return (
    <nav className="language-switcher" aria-label={label}>
      {options.map((option) => (
        <a
          aria-current={option.locale === currentLocale ? "page" : undefined}
          aria-label={languageLabels[option.locale]}
          className={option.locale === currentLocale ? "selected" : undefined}
          href={`/${option.locale}`}
          hrefLang={option.hrefLang}
          key={option.locale}
          lang={option.hrefLang}
          onClick={preserveLocation}
        >
          {option.label}
        </a>
      ))}
    </nav>
  );
}
