(function (window, document) {
  "use strict";

  var key = "paje-theme";
  var root = document.documentElement;

  function effectiveDark() {
    var explicit = root.getAttribute("data-color-scheme");
    if (explicit === "dark" || explicit === "light") {
      return explicit === "dark";
    }
    return Boolean(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
  }

  function updateButtons(dark) {
    document.querySelectorAll("[data-theme-toggle]").forEach(function (button) {
      button.textContent = dark ? "Light" : "Dark";
      button.setAttribute("aria-label", dark ? "Switch to light mode" : "Switch to dark mode");
    });
  }

  function apply(theme, save) {
    var dark = theme === "dark";
    root.classList.toggle("dark", dark);
    root.setAttribute("data-color-scheme", theme);
    if (save) {
      try {
        window.localStorage.setItem(key, theme);
      } catch {
        // The visible preference still applies for this page when storage is unavailable.
      }
    }
    root.setAttribute("data-theme-source", "preference");
    updateButtons(dark);
  }

  var saved;
  try {
    saved = window.localStorage.getItem(key);
  } catch {
    saved = null;
  }
  if (saved === "dark" || saved === "light") {
    apply(saved, false);
  } else {
    updateButtons(effectiveDark());
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("[data-theme-toggle]").forEach(function (button) {
      button.addEventListener("click", function () {
        apply(effectiveDark() ? "light" : "dark", true);
      });
    });
  });
})(window, document);
