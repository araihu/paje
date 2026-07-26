import { catalogs, type Locale } from "./i18n/catalogs";
import { LanguageSwitcher } from "./i18n/language-switcher";

const githubUrl = "https://github.com/araihu/paje";

const installCommands = `git clone https://github.com/araihu/paje.git
cd paje

PAJE_COMMIT="$(git rev-parse --verify 'HEAD^{commit}')"
docker build \\
  --build-arg CODEX_VERSION=0.144.5 \\
  --build-arg PAJE_COMMIT="$PAJE_COMMIT" \\
  -t ghcr.io/your-org/paje:beta .

helm upgrade --install paje ./charts/paje \\
  --namespace paje --create-namespace \\
  --values values.production.yaml`;

const phaseNames = ["Resolve", "Execute", "Approval", "Publish", "Finalize"] as const;
const guideAnchors = ["preparar", "instalar", "executar", "aprovar"] as const;
const docHrefs = [
  `${githubUrl}#workflow-input`,
  `${githubUrl}#configuration`,
  `${githubUrl}#kubernetes-deployment`,
  `${githubUrl}/blob/main/docs/superpowers/specs/2026-07-24-beta-code-change-workflow-design.md`,
] as const;

function workflowInput(locale: Locale) {
  const copy = catalogs[locale].guide.execute;

  return JSON.stringify(
    {
      run_id: "73f659b2-eaee-4c37-8784-48fab274b3e8",
      input: {
        idempotency_key: "client-timeout-20260725",
        task_description: copy.taskDescription,
        repository_uri: "https://github.com/example/service.git",
        base_ref: "main",
        memory_query: copy.memoryQuery,
        memory_limit: 5,
        tags: {
          user_id: "operator@example.com",
          app_id: "service",
        },
        profile: "generic",
        checks: [
          {
            name: "test",
            executable: "npm",
            args: ["test"],
            timeout: "10m",
            required: true,
          },
        ],
        publication: {
          mode: "artifact",
        },
      },
    },
    null,
    2,
  );
}

function Arrow() {
  return <span aria-hidden="true">↗</span>;
}

function BetaCopy({ text }: { text: string }) {
  const [before, after] = text.split("code-change@v1");

  return (
    <>
      {before}
      <code>code-change@v1</code>
      {after}
    </>
  );
}

export function LocalizedHome({ locale }: { locale: Locale }) {
  const copy = catalogs[locale];

  return (
    <div className="site-shell">
      <header className="topbar">
        <a className="brand" href="#inicio" aria-label={copy.navigation.brandLabel}>
          <span className="brand-mark" aria-hidden="true">P/</span>
          <span>Pajé</span>
        </a>
        <nav className="desktop-nav" aria-label={copy.navigation.primaryLabel}>
          <a href="#produto">{copy.navigation.product}</a>
          <a href="#fluxo">{copy.navigation.workflow}</a>
          <a href="#guia">{copy.navigation.guide}</a>
          <a href="#docs">{copy.navigation.docs}</a>
        </nav>
        <div className="topbar-actions">
          <LanguageSwitcher
            currentLocale={locale}
            label={copy.languageSwitcher.label}
            languageLabels={{
              en: copy.languageSwitcher.english,
              "pt-br": copy.languageSwitcher.portuguese,
              es: copy.languageSwitcher.spanish,
            }}
          />
          <a className="github-button" href={githubUrl} target="_blank" rel="noreferrer">
            GitHub <Arrow />
          </a>
        </div>
      </header>

      <main>
        <section className="hero" id="inicio">
          <div className="hero-copy">
            <div className="status-pill">
              <span className="status-dot" aria-hidden="true" />
              {copy.hero.status}
            </div>
            <p className="kicker">{copy.hero.kicker}</p>
            <h1>
              {copy.hero.titleLead}
              <span>{copy.hero.titleAccent}</span>
            </h1>
            <p className="hero-lead">{copy.hero.lead}</p>
            <div className="hero-actions">
              <a className="primary-button" href="#guia">
                {copy.hero.startGuide} <span aria-hidden="true">↓</span>
              </a>
              <a className="text-button" href={`${githubUrl}#readme`} target="_blank" rel="noreferrer">
                {copy.hero.readGitHub} <Arrow />
              </a>
            </div>
            <div className="hero-note" aria-label={copy.hero.principlesLabel}>
              {copy.hero.principles.map((principle, index) => (
                <span className="hero-principle" key={principle}>
                  {index > 0 ? <i aria-hidden="true" /> : null}
                  <span>{principle}</span>
                </span>
              ))}
            </div>
          </div>

          <div className="run-card" aria-label={copy.run.ariaLabel} data-stamp={copy.run.stamp}>
            <div className="run-card-top">
              <div>
                <span className="mono-label">RUN / CODE-CHANGE@V1</span>
                <strong>client-timeout-20260725</strong>
              </div>
              <span className="run-state"><i aria-hidden="true" /> {copy.run.state}</span>
            </div>
            <div className="run-flow">
              {phaseNames.map((phase, index) => (
                <div className={`run-step ${index < 2 ? "complete" : index === 2 ? "active" : "locked"}`} key={phase}>
                  <span className="step-icon" aria-hidden="true">{index < 2 ? "✓" : index === 2 ? "•" : "○"}</span>
                  <span>{phase.toLowerCase()}</span>
                  <small>{index < 2 ? copy.run.completed : index === 2 ? copy.run.decisionNeeded : copy.run.queued}</small>
                </div>
              ))}
            </div>
            <div className="artifact-card">
              <div className="artifact-heading">
                <span>{copy.run.artifact}</span>
                <span className="artifact-size">42.8 KB</span>
              </div>
              <code>sha256:8d7a4e...bf31</code>
              <div className="artifact-grid">
                <span><b>7</b> {copy.run.files}</span>
                <span><b>18</b> {copy.run.checks}</span>
                <span><b>0</b> {copy.run.secrets}</span>
              </div>
            </div>
            <div className="run-card-footer">
              <span><i className="pulse" aria-hidden="true" /> {copy.run.persisted}</span>
              <code>base 52ce223</code>
            </div>
          </div>
        </section>

        <section className="proof-bar" aria-label={copy.proof.ariaLabel}>
          {copy.proof.items.map((item) => (
            <div key={item.value}><strong>{item.value}</strong><span>{item.label}</span></div>
          ))}
        </section>

        <section className="section features-section" id="produto">
          <div className="section-heading split-heading">
            <div>
              <p className="kicker">{copy.featuresHeading.kicker}</p>
              <h2>{copy.featuresHeading.titleLead}<br />{copy.featuresHeading.titleEnd}</h2>
            </div>
            <p>{copy.featuresHeading.text}</p>
          </div>
          <div className="feature-grid">
            {copy.features.map((feature, index) => (
              <article className="feature-card" key={feature.title}>
                <div className="feature-topline">
                  <span>{String(index + 1).padStart(2, "0")}</span>
                  <code>{feature.signal}</code>
                </div>
                <h3>{feature.title}</h3>
                <p>{feature.text}</p>
              </article>
            ))}
          </div>
        </section>

        <section className="section flow-section" id="fluxo">
          <div className="section-heading flow-heading">
            <p className="kicker">code-change@v1</p>
            <h2>{copy.workflow.titleLead}<br />{copy.workflow.titleEnd}</h2>
            <p className="section-intro">{copy.workflow.intro}</p>
          </div>
          <div className="phase-list">
            {copy.workflow.phases.map((phase, index) => (
              <article className="phase-row" key={phaseNames[index]}>
                <span className="phase-number">{String(index + 1).padStart(2, "0")}</span>
                <div>
                  <p>{phaseNames[index]}</p>
                  <h3>{phase.label}</h3>
                </div>
                <p className="phase-detail">{phase.detail}</p>
              </article>
            ))}
          </div>
        </section>

        <section className="section guide-section" id="guia">
          <div className="section-heading guide-heading">
            <div>
              <p className="kicker">{copy.guide.kicker}</p>
              <h2>{copy.guide.title}</h2>
            </div>
            <p>{copy.guide.intro}</p>
          </div>

          <div className="guide-grid">
            <aside className="guide-steps" aria-label={copy.guide.stepsLabel}>
              {copy.guide.steps.map((step, index) => (
                <a href={`#${guideAnchors[index]}`} key={step.title}>
                  <span>{String(index + 1).padStart(2, "0")}</span>
                  <b>{step.title}</b>
                  <small>{step.detail}</small>
                </a>
              ))}
            </aside>

            <div className="guide-content">
              <article className="guide-panel" id="preparar">
                <div className="guide-panel-heading">
                  <span>01</span>
                  <div><p>{copy.guide.prepare.eyebrow}</p><h3>{copy.guide.prepare.title}</h3></div>
                </div>
                <p>{copy.guide.prepare.text}</p>
                <div className="requirement-list">
                  {copy.guide.prepare.requirements.map((requirement) => <span key={requirement}>{requirement}</span>)}
                </div>
              </article>

              <article className="guide-panel" id="instalar">
                <div className="guide-panel-heading">
                  <span>02</span>
                  <div><p>{copy.guide.install.eyebrow}</p><h3>{copy.guide.install.title}</h3></div>
                </div>
                <p>{copy.guide.install.text}</p>
                <div className="code-window">
                  <div className="code-title"><span>terminal</span><code>bash</code></div>
                  <pre><code>{installCommands}</code></pre>
                </div>
                <a className="inline-link" href={`${githubUrl}#kubernetes-deployment`} target="_blank" rel="noreferrer">
                  {copy.guide.install.link} <Arrow />
                </a>
              </article>

              <article className="guide-panel" id="executar">
                <div className="guide-panel-heading">
                  <span>03</span>
                  <div><p>{copy.guide.execute.eyebrow}</p><h3>{copy.guide.execute.title}</h3></div>
                </div>
                <p>
                  {copy.guide.execute.beforeWorkflow} <code>paje-code-change-v1</code> {copy.guide.execute.afterWorkflow}{" "}
                  <code>profile: generic</code> {copy.guide.execute.afterProfile}{" "}
                  <code>go</code> {copy.guide.execute.afterGoProfile}
                </p>
                <div className="code-window code-window-light">
                  <div className="code-title"><span>workflow-input.json</span><code>json</code></div>
                  <pre><code>{workflowInput(locale)}</code></pre>
                </div>
              </article>

              <article className="guide-panel" id="aprovar">
                <div className="guide-panel-heading">
                  <span>04</span>
                  <div><p>{copy.guide.approve.eyebrow}</p><h3>{copy.guide.approve.title}</h3></div>
                </div>
                <p>
                  {copy.guide.approve.beforeMode} <code>publication.mode: pull_request</code>. {copy.guide.approve.afterMode}{" "}
                  <code>paje:approval:&lt;run-id&gt;</code> {copy.guide.approve.afterEvent}
                </p>
                <a className="inline-link" href={`${githubUrl}#approval-event`} target="_blank" rel="noreferrer">
                  {copy.guide.approve.link} <Arrow />
                </a>
              </article>
            </div>
          </div>
        </section>

        <section className="section security-section" id="seguranca">
          <div className="security-copy">
            <p className="kicker">{copy.security.kicker}</p>
            <h2>{copy.security.titleLead}<br />{copy.security.titleEnd}</h2>
            <p>{copy.security.text}</p>
            <a className="text-button light" href={`${githubUrl}#idempotency-and-conflict-behavior`} target="_blank" rel="noreferrer">
              {copy.security.link} <Arrow />
            </a>
          </div>
          <div className="boundary-diagram" aria-label={copy.security.diagramLabel}>
            <div className="boundary-top">
              <span>{copy.security.boundary}</span>
              <code>{copy.security.credentials}</code>
            </div>
            <div className="boundary-agent">
              <div><span className="mini-mark">P/</span><strong>{copy.security.runtime}</strong></div>
              <ul>
                <li><span>✓</span> {copy.security.selectedMemory}</li>
                <li><span>✓</span> {copy.security.isolatedWorktree}</li>
                <li><span>✓</span> {copy.security.allowedEnvironment}</li>
                <li className="denied"><span>×</span> {copy.security.workerTokens}</li>
              </ul>
            </div>
            <div className="boundary-footer">
              <span>codex home</span><span>git publisher</span><span>artifact store</span>
            </div>
          </div>
        </section>

        <section className="section docs-section" id="docs">
          <div className="section-heading docs-heading">
            <div>
              <p className="kicker">{copy.docs.kicker}</p>
              <h2>{copy.docs.titleLead}<br />{copy.docs.titleEnd}</h2>
            </div>
            <a className="text-button" href={`${githubUrl}#readme`} target="_blank" rel="noreferrer">
              {copy.docs.openReadme} <Arrow />
            </a>
          </div>
          <div className="docs-grid">
            {copy.docs.cards.map((doc, index) => (
              <a className="doc-card" href={docHrefs[index]} target="_blank" rel="noreferrer" key={doc.title}>
                <p>{doc.eyebrow}</p>
                <h3>{doc.title}</h3>
                <span>{doc.text}</span>
                <b aria-hidden="true">↗</b>
              </a>
            ))}
          </div>
          <div className="beta-note">
            <div className="beta-stamp"><span>Beta</span><small>{copy.docs.betaScope}</small></div>
            <p><BetaCopy text={copy.docs.betaText} /></p>
          </div>
        </section>

        <section className="closing-section">
          <p className="kicker">{copy.closing.kickerLead}<br />{copy.closing.kickerEnd}</p>
          <h2>{copy.closing.titleLead}<br />{copy.closing.titleEnd}</h2>
          <div className="closing-actions">
            <a className="primary-button inverse" href={githubUrl} target="_blank" rel="noreferrer">
              {copy.closing.viewGitHub} <Arrow />
            </a>
            <a className="text-button light" href="#guia">{copy.closing.revisitGuide} <span aria-hidden="true">↑</span></a>
          </div>
        </section>
      </main>

      <footer>
        <a className="brand footer-brand" href="#inicio" aria-label={copy.footer.brandLabel}>
          <span className="brand-mark" aria-hidden="true">P/</span><span>Pajé</span>
        </a>
        <p>{copy.footer.text}</p>
        <div><span>Open source · MIT</span><a href={githubUrl} target="_blank" rel="noreferrer">GitHub <Arrow /></a></div>
      </footer>
    </div>
  );
}

export default function Home() {
  return <LocalizedHome locale="en" />;
}
