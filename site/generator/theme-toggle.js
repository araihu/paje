(function (window, document) {
  "use strict";

  var key = "paje-theme";
  var root = document.documentElement;

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
    document.querySelectorAll("[data-theme-toggle]").forEach(function (button) {
      button.textContent = dark ? "Light" : "Dark";
      button.setAttribute("aria-label", dark ? "Switch to light mode" : "Switch to dark mode");
    });
  }

  var saved;
  try {
    saved = window.localStorage.getItem(key);
  } catch {
    saved = null;
  }
  if (saved === "dark" || saved === "light") {
    apply(saved, false);
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("[data-theme-toggle]").forEach(function (button) {
      button.addEventListener("click", function () {
        apply(root.classList.contains("dark") ? "light" : "dark", true);
      });
    });
  });
})(window, document);
