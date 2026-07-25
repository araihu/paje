const githubUrl = "https://github.com/araihu/paje";

const workflowInput = `{
  "run_id": "73f659b2-eaee-4c37-8784-48fab274b3e8",
  "input": {
    "idempotency_key": "client-timeout-20260725",
    "task_description": "Aumente o timeout do cliente e atualize os testes.",
    "repository_uri": "https://github.com/example/service.git",
    "base_ref": "main",
    "memory_query": "convenções do cliente HTTP",
    "memory_limit": 5,
    "tags": {
      "user_id": "operator@example.com",
      "app_id": "service"
    },
    "profile": "generic",
    "checks": [
      {
        "name": "test",
        "executable": "npm",
        "args": ["test"],
        "timeout": "10m",
        "required": true
      }
    ],
    "publication": {
      "mode": "artifact"
    }
  }
}`;

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

const features = [
  {
    number: "01",
    title: "Desenhado para o agente pilotar",
    text: "O modelo de produto coloca hooks e skills do harness na entrada, para o próprio agente acionar o protocolo, acompanhar o run e retomar com contexto.",
    signal: "product direction",
  },
  {
    number: "02",
    title: "Linguagem neutra",
    text: "O profile generic executa checks estruturados para qualquer stack disponível na imagem do worker. Go é a implementação e um profile opcional, não um requisito do repositório.",
    signal: "any stack",
  },
  {
    number: "03",
    title: "Execução durável",
    text: "Cada fase persiste seu estado. Retries retomam o trabalho certo, sem relançar o agente ou duplicar efeitos.",
    signal: "restart-safe",
  },
  {
    number: "04",
    title: "Memória com escopo",
    text: "O agente recebe contexto relevante por usuário e aplicação. Credenciais do worker ficam fora da execução.",
    signal: "Mem0 adapter",
  },
  {
    number: "05",
    title: "Artefato verificável",
    text: "Patch, saída, verificações e preflight viram um bundle imutável, autenticado por SHA-256.",
    signal: "content-addressed",
  },
  {
    number: "06",
    title: "Aprovação vinculada",
    text: "A decisão humana vale para um run e um digest exatos. Mudou o artefato? A aprovação deixa de valer.",
    signal: "artifact-bound",
  },
  {
    number: "07",
    title: "PR determinístico",
    text: "Branch, commit e pull request são reutilizados apenas quando todos os vínculos conferem. Sem force-push.",
    signal: "idempotent",
  },
  {
    number: "08",
    title: "Harness substituível",
    text: "Codex é o primeiro harness suportado. A fronteira de execução existe para receber outros harnesses sem mudar o protocolo durável.",
    signal: "Codex first",
  },
];

const phases = [
  {
    name: "Resolve",
    label: "Fixar a verdade",
    detail: "Valida a entrada, reserva a chave idempotente e resolve o ref para um commit imutável.",
  },
  {
    name: "Execute",
    label: "Fazer e provar",
    detail: "Prepara um worktree isolado, recupera memória, roda o agente e verifica cada módulo selecionado.",
  },
  {
    name: "Approval",
    label: "Pedir decisão",
    detail: "Pausa de forma durável e espera uma aprovação ligada ao digest exato do artefato.",
  },
  {
    name: "Publish",
    label: "Publicar sem surpresa",
    detail: "Reaplica e reverifica o bundle antes de criar ou reutilizar um draft PR determinístico.",
  },
  {
    name: "Finalize",
    label: "Fechar o ciclo",
    detail: "Grava um único resultado na memória e retorna referências duráveis para auditoria.",
  },
];

const docs = [
  {
    eyebrow: "01 / Use",
    title: "Contrato do agente",
    text: "Envelope que hooks e skills enviam, com profiles, checks e modos de publicação do code-change@v1.",
    href: `${githubUrl}#workflow-input`,
  },
  {
    eyebrow: "02 / Operate",
    title: "Configuração",
    text: "Adapters, limites, ambiente filtrado e contratos das variáveis do worker.",
    href: `${githubUrl}#configuration`,
  },
  {
    eyebrow: "03 / Deploy",
    title: "Kubernetes + Helm",
    text: "Imagem, Secrets separados, persistência e valores para operar o beta.",
    href: `${githubUrl}#kubernetes-deployment`,
  },
  {
    eyebrow: "04 / Understand",
    title: "Design do beta",
    text: "Decisões, fronteiras e invariantes de segurança do fluxo de code change.",
    href: `${githubUrl}/blob/main/docs/superpowers/specs/2026-07-24-beta-code-change-workflow-design.md`,
  },
];

function Arrow() {
  return <span aria-hidden="true">↗</span>;
}

export default function Home() {
  return (
    <div className="site-shell">
      <header className="topbar">
        <a className="brand" href="#inicio" aria-label="Pajé — início">
          <span className="brand-mark" aria-hidden="true">P/</span>
          <span>Pajé</span>
        </a>
        <nav className="desktop-nav" aria-label="Navegação principal">
          <a href="#produto">Produto</a>
          <a href="#fluxo">Como funciona</a>
          <a href="#guia">Guia de uso</a>
          <a href="#docs">Docs</a>
        </nav>
        <a className="github-button" href={githubUrl} target="_blank" rel="noreferrer">
          GitHub <Arrow />
        </a>
      </header>

      <main>
        <section className="hero" id="inicio">
          <div className="hero-copy">
            <div className="status-pill">
              <span className="status-dot" aria-hidden="true" />
              Beta disponível · Codex é o primeiro harness
            </div>
            <p className="kicker">Orquestração durável pilotada pelo agente</p>
            <h1>
              Do pedido ao pull request.
              <span> Sem perder o fio.</span>
            </h1>
            <p className="hero-lead">
              Pajé foi desenhado para ser pilotado pelo próprio agente via hooks e skills. Ele transforma cada mudança
              em um fluxo verificável, independente da linguagem: contexto certo, execução isolada, aprovação humana e
              publicação idempotente.
            </p>
            <div className="hero-actions">
              <a className="primary-button" href="#guia">Começar pelo guia <span aria-hidden="true">↓</span></a>
              <a className="text-button" href={`${githubUrl}#readme`} target="_blank" rel="noreferrer">
                Ler no GitHub <Arrow />
              </a>
            </div>
            <div className="hero-note" aria-label="Princípios e suporte atual">
              <span>Agent-piloted design</span><i aria-hidden="true" />
              <span>Hooks + skills</span><i aria-hidden="true" />
              <span>Language-neutral</span><i aria-hidden="true" />
              <span>Codex first</span>
            </div>
          </div>

          <div className="run-card" aria-label="Exemplo de execução do Pajé">
            <div className="run-card-top">
              <div>
                <span className="mono-label">RUN / CODE-CHANGE@V1</span>
                <strong>client-timeout-20260725</strong>
              </div>
              <span className="run-state"><i aria-hidden="true" /> aguardando aprovação</span>
            </div>
            <div className="run-flow">
              {phases.map((phase, index) => (
                <div className={`run-step ${index < 2 ? "complete" : index === 2 ? "active" : "locked"}`} key={phase.name}>
                  <span className="step-icon" aria-hidden="true">{index < 2 ? "✓" : index === 2 ? "•" : "○"}</span>
                  <span>{phase.name.toLowerCase()}</span>
                  <small>{index < 2 ? "concluído" : index === 2 ? "decisão necessária" : "na fila"}</small>
                </div>
              ))}
            </div>
            <div className="artifact-card">
              <div className="artifact-heading">
                <span>Artefato verificado</span>
                <span className="artifact-size">42.8 KB</span>
              </div>
              <code>sha256:8d7a4e...bf31</code>
              <div className="artifact-grid">
                <span><b>7</b> arquivos</span>
                <span><b>18</b> checks</span>
                <span><b>0</b> segredos</span>
              </div>
            </div>
            <div className="run-card-footer">
              <span><i className="pulse" aria-hidden="true" /> estado persistido</span>
              <code>base 52ce223</code>
            </div>
          </div>
        </section>

        <section className="proof-bar" aria-label="Princípios e estado do beta">
          <div><strong>Agent</strong><span>modelo: hooks + skills</span></div>
          <div><strong>Generic</strong><span>checks para qualquer stack</span></div>
          <div><strong>Codex</strong><span>primeiro harness</span></div>
          <div><strong>5</strong><span>fases duráveis</span></div>
        </section>

        <section className="section features-section" id="produto">
          <div className="section-heading split-heading">
            <div>
              <p className="kicker">O agente pilota. Pajé sustenta.</p>
              <h2>Autonomia sem<br />perder o processo.</h2>
            </div>
            <p>
              O harness preserva a autonomia do agente; Pajé oferece o contrato previsível para os efeitos. Hooks e
              skills são a superfície de integração pretendida. Cada ação deixa uma prova e cada retry sabe de onde continuar.
            </p>
          </div>
          <div className="feature-grid">
            {features.map((feature) => (
              <article className="feature-card" key={feature.number}>
                <div className="feature-topline">
                  <span>{feature.number}</span>
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
            <h2>Um fluxo que sabe<br />onde está.</h2>
            <p className="section-intro">
              O agente inicia e acompanha o trabalho pela integração do harness. Hatchet cuida da fila, retries e sinais;
              Pajé cuida do contrato: estado, contexto, artefatos, política e publicação.
            </p>
          </div>
          <div className="phase-list">
            {phases.map((phase, index) => (
              <article className="phase-row" key={phase.name}>
                <span className="phase-number">0{index + 1}</span>
                <div>
                  <p>{phase.name}</p>
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
              <p className="kicker">Guia rápido</p>
              <h2>Do trigger ao primeiro run.</h2>
            </div>
            <p>O beta é um worker self-hosted. Codex é o primeiro harness; o protocolo foi desenhado para receber outros.</p>
          </div>

          <div className="guide-grid">
            <aside className="guide-steps" aria-label="Passos do guia">
              <a href="#preparar"><span>01</span><b>Preparar</b><small>imagem + secrets</small></a>
              <a href="#instalar"><span>02</span><b>Instalar</b><small>Helm + adapters</small></a>
              <a href="#executar"><span>03</span><b>Executar</b><small>trigger → workflow</small></a>
              <a href="#aprovar"><span>04</span><b>Aprovar</b><small>quando houver PR</small></a>
            </aside>

            <div className="guide-content">
              <article className="guide-panel" id="preparar">
                <div className="guide-panel-heading">
                  <span>01</span>
                  <div><p>Pré-requisitos</p><h3>Prepare o terreno</h3></div>
                </div>
                <p>
                  Go 1.26+ é necessário apenas para desenvolver o Pajé, que é implementado em Go. O repositório atendido
                  pode usar qualquer linguagem; inclua o toolchain correspondente na imagem do worker. No beta, prepare
                  Docker, Helm 3, Hatchet e autenticação do Codex.
                </p>
                <div className="requirement-list">
                  <span>Qualquer stack</span><span>Docker</span><span>Helm 3</span><span>Hatchet</span><span>Codex · primeiro harness</span>
                </div>
              </article>

              <article className="guide-panel" id="instalar">
                <div className="guide-panel-heading">
                  <span>02</span>
                  <div><p>Instalação</p><h3>Construa e instale</h3></div>
                </div>
                <p>Construa a imagem com uma revisão auditável e aplique o chart depois de configurar Secrets separados.</p>
                <div className="code-window">
                  <div className="code-title"><span>terminal</span><code>bash</code></div>
                  <pre><code>{installCommands}</code></pre>
                </div>
                <a className="inline-link" href={`${githubUrl}#kubernetes-deployment`} target="_blank" rel="noreferrer">
                  Ver instalação completa e valores do Helm <Arrow />
                </a>
              </article>

              <article className="guide-panel" id="executar">
                <div className="guide-panel-heading">
                  <span>03</span>
                  <div><p>Primeira execução</p><h3>Dispare um artifact run</h3></div>
                </div>
                <p>
                  Hoje, inicie <code>paje-code-change-v1</code> no Hatchet. A integração pretendida levará esse trigger para
                  hooks e skills do harness. Use <code>profile: generic</code> com checks explícitos e o toolchain presente
                  na imagem. O profile <code>go</code> é só uma conveniência para descoberta de módulos e defaults de teste.
                </p>
                <div className="code-window code-window-light">
                  <div className="code-title"><span>workflow-input.json</span><code>json</code></div>
                  <pre><code>{workflowInput}</code></pre>
                </div>
              </article>

              <article className="guide-panel" id="aprovar">
                <div className="guide-panel-heading">
                  <span>04</span>
                  <div><p>Publicação</p><h3>Aprove o artefato, não a intenção</h3></div>
                </div>
                <p>
                  Para criar um draft PR, use <code>publication.mode: pull_request</code>. A fase de aprovação aguarda o
                  evento <code>paje:approval:&lt;run-id&gt;</code> com o mesmo run e digest. Se algo mudar, a decisão não é reaproveitada.
                </p>
                <a className="inline-link" href={`${githubUrl}#approval-event`} target="_blank" rel="noreferrer">
                  Ver contrato do evento de aprovação <Arrow />
                </a>
              </article>
            </div>
          </div>
        </section>

        <section className="section security-section" id="seguranca">
          <div className="security-copy">
            <p className="kicker">Segurança por fronteiras</p>
            <h2>O agente vê o necessário.<br />O sistema guarda o sensível.</h2>
            <p>
              Credenciais de Hatchet, Mem0 e GitHub não entram no ambiente do agente. Publicação acontece em um repositório
              bare novo e confiável, depois da verificação do código controlado pelo repositório.
            </p>
            <a className="text-button light" href={`${githubUrl}#idempotency-and-conflict-behavior`} target="_blank" rel="noreferrer">
              Ler garantias e conflitos <Arrow />
            </a>
          </div>
          <div className="boundary-diagram" aria-label="Fronteiras de credenciais">
            <div className="boundary-top">
              <span>worker boundary</span>
              <code>service credentials</code>
            </div>
            <div className="boundary-agent">
              <div><span className="mini-mark">P/</span><strong>Agent runtime</strong></div>
              <ul>
                <li><span>✓</span> memória selecionada</li>
                <li><span>✓</span> worktree isolado</li>
                <li><span>✓</span> ambiente permitido</li>
                <li className="denied"><span>×</span> tokens do worker</li>
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
              <p className="kicker">Documentação</p>
              <h2>Entenda o contrato.<br />Opere com confiança.</h2>
            </div>
            <a className="text-button" href={`${githubUrl}#readme`} target="_blank" rel="noreferrer">
              Abrir README completo <Arrow />
            </a>
          </div>
          <div className="docs-grid">
            {docs.map((doc) => (
              <a className="doc-card" href={doc.href} target="_blank" rel="noreferrer" key={doc.title}>
                <p>{doc.eyebrow}</p>
                <h3>{doc.title}</h3>
                <span>{doc.text}</span>
                <b aria-hidden="true">↗</b>
              </a>
            ))}
          </div>
          <div className="beta-note">
            <div className="beta-stamp"><span>Beta</span><small>scope</small></div>
            <p>
              Hoje, Pajé oferece o template tipado <code>code-change@v1</code>, o runner do Codex como primeiro harness,
              disparo pelo Hatchet, modo artifact ou draft PR no GitHub e uma única réplica. Hooks e skills agent-side e
              outros harnesses serão suportados no futuro. Merge automático, YAML arbitrário, releases e múltiplas réplicas
              ficam fora deste beta.
            </p>
          </div>
        </section>

        <section className="closing-section">
          <p className="kicker">O agente pilota via hooks e skills.<br />Pajé torna o percurso durável.</p>
          <h2>Autonomia para qualquer stack.<br />Codex primeiro, mais harnesses depois.</h2>
          <div className="closing-actions">
            <a className="primary-button inverse" href={githubUrl} target="_blank" rel="noreferrer">
              Ver projeto no GitHub <Arrow />
            </a>
            <a className="text-button light" href="#guia">Rever o guia <span aria-hidden="true">↑</span></a>
          </div>
        </section>
      </main>

      <footer>
        <a className="brand footer-brand" href="#inicio"><span className="brand-mark">P/</span><span>Pajé</span></a>
        <p>Orquestração durável, pilotada pelo agente e independente da linguagem.</p>
        <div><span>Open source · MIT</span><a href={githubUrl} target="_blank" rel="noreferrer">GitHub <Arrow /></a></div>
      </footer>
    </div>
  );
}
