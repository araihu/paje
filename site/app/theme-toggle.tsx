"use client";

import { useState } from "react";

export function ThemeToggle() {
  const [dark, setDark] = useState(false);

  function toggle() {
    const next = !dark;
    document.documentElement.classList.toggle("dark", next);
    setDark(next);
  }

  return <button className="theme-toggle" type="button" aria-label={dark ? "Switch to light mode" : "Switch to dark mode"} onClick={toggle}>{dark ? "Light" : "Dark"}</button>;
}
