export function ThemeToggle() {
  return <button className="theme-toggle" type="button" aria-label="Switch to dark mode" data-theme-toggle>Dark</button>;
}

export function CampaignToggle() {
  return (
    <button
      aria-label="Toggle seasonal appearance"
      aria-pressed="false"
      className="campaign-toggle"
      data-campaign-toggle
      hidden
      type="button"
    >
      <span aria-hidden="true" data-campaign-toggle-icon />
    </button>
  );
}
