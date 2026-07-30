import { createElement, Fragment } from "react";

export const seasonalRuntimeURL = "https://araihu.com/assets/campaign/v1.js";
export const seasonalChannelURL = "https://araihu.com/assets/releases/current";
export const seasonalRuntimeSRI = "sha384-oPH7l1vK9vKP1Dn+18sO3yEXlz4ts6KzPEQl0SW4Y/+im05gOaamNNaQAf6bGH/n";
export const pajeLogoFallback =
  "https://araihu.com/assets/releases/v0.1.1/brand/paje/logo/adaptive-plate-optical.svg";
export const seasonalRootAttributes = {
  "data-theme": "araihu",
  "data-theme-source": "default",
};

export function SeasonalScripts() {
  return createElement(
    Fragment,
    null,
    createElement("script", { defer: true, src: "/theme-toggle.js" }),
    createElement("script", {
      crossOrigin: "anonymous",
      "data-channel": seasonalChannelURL,
      defer: true,
      integrity: seasonalRuntimeSRI,
      src: seasonalRuntimeURL,
    }),
  );
}

export function ManagedPajeLogo({ className }) {
  return createElement("img", {
    alt: "",
    className,
    crossOrigin: "anonymous",
    "data-asset-brand": "logo",
    height: 41,
    src: pajeLogoFallback,
    width: 166,
  });
}

export function ThemeToggle() {
  return createElement(
    "button",
    {
      "aria-label": "Switch to dark mode",
      className: "theme-toggle",
      "data-theme-toggle": "",
      type: "button",
    },
    "Dark",
  );
}

export function CampaignToggle() {
  return createElement(
    "button",
    {
      "aria-label": "Toggle seasonal appearance",
      "aria-pressed": "false",
      className: "campaign-toggle",
      "data-campaign-toggle": "",
      hidden: true,
      type: "button",
    },
    createElement("span", { "aria-hidden": "true", "data-campaign-toggle-icon": "" }),
  );
}
