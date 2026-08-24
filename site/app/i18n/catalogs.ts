export const locales = ["en", "pt-br", "es"] as const;

export type Locale = (typeof locales)[number];

export interface SiteCatalog {
  htmlLang: "en" | "pt-BR" | "es";
  metadata: {
    title: string;
    description: string;
    openGraphTitle: string;
    openGraphDescription: string;
    openGraphLocale: "en_US" | "pt_BR" | "es_ES";
    socialImageAlt: string;
  };
  languageSwitcher: {
    label: string;
    english: string;
    portuguese: string;
    spanish: string;
  };
  navigation: {
    brandLabel: string;
    primaryLabel: string;
    product: string;
    workflow: string;
    guide: string;
    docs: string;
  };
  hero: {
    status: string;
    kicker: string;
    titleLead: string;
    titleAccent: string;
    lead: string;
    startGuide: string;
    readGitHub: string;
    principlesLabel: string;
    principles: readonly [string, string, string, string];
  };
  run: {
    ariaLabel: string;
    stamp: string;
    state: string;
    completed: string;
    decisionNeeded: string;
    queued: string;
    artifact: string;
    files: string;
    checks: string;
    secrets: string;
    persisted: string;
  };
  proof: {
    ariaLabel: string;
    items: readonly [
      { value: string; label: string },
      { value: string; label: string },
      { value: string; label: string },
      { value: string; label: string },
    ];
  };
  featuresHeading: {
    kicker: string;
    titleLead: string;
    titleEnd: string;
    text: string;
  };
  features: readonly {
    title: string;
    text: string;
    signal: string;
  }[];
  workflow: {
    titleLead: string;
    titleEnd: string;
    intro: string;
    phases: readonly {
      label: string;
      detail: string;
    }[];
  };
  guide: {
    kicker: string;
    title: string;
    intro: string;
    stepsLabel: string;
    steps: readonly {
      title: string;
      detail: string;
    }[];
    prepare: {
      eyebrow: string;
      title: string;
      text: string;
      requirements: readonly string[];
    };
    install: {
      eyebrow: string;
      title: string;
      text: string;
      link: string;
    };
    execute: {
      eyebrow: string;
      title: string;
      beforeWorkflow: string;
      afterWorkflow: string;
      afterProfile: string;
      afterGoProfile: string;
      taskDescription: string;
      memoryQuery: string;
    };
    approve: {
      eyebrow: string;
      title: string;
      beforeMode: string;
      afterMode: string;
      afterEvent: string;
      link: string;
    };
  };
  security: {
    kicker: string;
    titleLead: string;
    titleEnd: string;
    text: string;
    link: string;
    diagramLabel: string;
    boundary: string;
    credentials: string;
    runtime: string;
    selectedMemory: string;
    isolatedWorktree: string;
    allowedEnvironment: string;
    workerTokens: string;
  };
  docs: {
    kicker: string;
    titleLead: string;
    titleEnd: string;
    openReadme: string;
    cards: readonly {
      eyebrow: string;
      title: string;
      text: string;
    }[];
    betaScope: string;
    betaText: string;
  };
  closing: {
    kickerLead: string;
    kickerEnd: string;
    titleLead: string;
    titleEnd: string;
    viewGitHub: string;
    revisitGuide: string;
  };
  footer: {
    brandLabel: string;
    text: string;
  };
}

const portuguese = {
  htmlLang: "pt-BR",
  metadata: {
    title: "Pajé — Orquestração durável pilotada pelo agente",
    description:
      "Projetado para o agente pilotar via hooks e skills. Pajé torna mudanças duráveis em qualquer linguagem, com Codex como primeiro harness.",
    openGraphTitle: "Pajé — Do pedido ao pull request. Sem perder o fio.",
    openGraphDescription:
      "Projetado para o agente pilotar, independente da linguagem e com Codex como primeiro harness.",
    openGraphLocale: "pt_BR",
    socialImageAlt: "Pajé — Do pedido ao pull request. Sem perder o fio.",
  },
  languageSwitcher: {
    label: "Selecionar idioma",
    english: "Ver em inglês",
    portuguese: "Ver em português do Brasil",
    spanish: "Ver em espanhol",
  },
  navigation: {
    brandLabel: "Pajé — início",
    primaryLabel: "Navegação principal",
    product: "Produto",
    workflow: "Como funciona",
    guide: "Guia de uso",
    docs: "Docs",
  },
  hero: {
    status: "Beta disponível · Codex é o primeiro harness",
    kicker: "Orquestração durável pilotada pelo agente",
    titleLead: "Do pedido ao pull request.",
    titleAccent: " Sem perder o fio.",
    lead:
      "Pajé foi desenhado para ser pilotado pelo próprio agente via hooks e skills. Ele transforma cada mudança em um fluxo verificável, independente da linguagem: contexto certo, execução isolada, aprovação humana e publicação idempotente.",
    startGuide: "Começar pelo guia",
    readGitHub: "Ler no GitHub",
    principlesLabel: "Princípios e suporte atual",
    principles: ["Agent-piloted design", "Hooks + skills", "Language-neutral", "Codex first"],
  },
  run: {
    ariaLabel: "Exemplo de execução do Pajé",
    stamp: "PERSISTED",
    state: "aguardando aprovação",
    completed: "concluído",
    decisionNeeded: "decisão necessária",
    queued: "na fila",
    artifact: "Artefato verificado",
    files: "arquivos",
    checks: "checks",
    secrets: "segredos",
    persisted: "estado persistido",
  },
  proof: {
    ariaLabel: "Princípios e estado do beta",
    items: [
      { value: "Agent", label: "modelo: hooks + skills" },
      { value: "Generic", label: "checks para qualquer stack" },
      { value: "Codex", label: "primeiro harness" },
      { value: "5", label: "fases duráveis" },
    ],
  },
  featuresHeading: {
    kicker: "O agente pilota. Pajé sustenta.",
    titleLead: "Autonomia sem",
    titleEnd: "perder o processo.",
    text:
      "O harness preserva a autonomia do agente; Pajé oferece o contrato previsível para os efeitos. Hooks e skills são a superfície de integração pretendida. Cada ação deixa uma prova e cada retry sabe de onde continuar.",
  },
  features: [
    {
      title: "Desenhado para o agente pilotar",
      text:
        "O modelo de produto coloca hooks e skills do harness na entrada, para o próprio agente acionar o protocolo, acompanhar o run e retomar com contexto.",
      signal: "product direction",
    },
    {
      title: "Linguagem neutra",
      text:
        "O profile generic executa checks estruturados para qualquer stack disponível na imagem do worker. Go é a implementação e um profile opcional, não um requisito do repositório.",
      signal: "any stack",
    },
    {
      title: "Execução durável",
      text:
        "Cada fase persiste seu estado. Retries retomam o trabalho certo, sem relançar o agente ou duplicar efeitos.",
      signal: "restart-safe",
    },
    {
      title: "Memória com escopo",
      text:
        "O agente recebe contexto relevante por usuário e aplicação. Credenciais do worker ficam fora da execução.",
      signal: "Mem0 adapter",
    },
    {
      title: "Artefato verificável",
      text:
        "Patch, saída, verificações e preflight viram um bundle imutável, autenticado por SHA-256.",
      signal: "content-addressed",
    },
    {
      title: "Aprovação vinculada",
      text:
        "A decisão humana vale para um run e um digest exatos. Mudou o artefato? A aprovação deixa de valer.",
      signal: "artifact-bound",
    },
    {
      title: "PR determinístico",
      text:
        "Branch, commit e pull request são reutilizados apenas quando todos os vínculos conferem. Sem force-push.",
      signal: "idempotent",
    },
    {
      title: "Harness substituível",
      text:
        "Codex é o primeiro harness suportado. A fronteira de execução existe para receber outros harnesses sem mudar o protocolo durável.",
      signal: "Codex first",
    },
  ],
  workflow: {
    titleLead: "Um fluxo que sabe",
    titleEnd: "onde está.",
    intro:
      "O agente inicia e acompanha o trabalho pela integração do harness. Hatchet cuida da fila, retries e sinais; Pajé cuida do contrato: estado, contexto, artefatos, política e publicação.",
    phases: [
      {
        label: "Fixar a verdade",
        detail:
          "Valida a entrada, reserva a chave idempotente e resolve o ref para um commit imutável.",
      },
      {
        label: "Fazer e provar",
        detail:
          "Prepara um worktree isolado, recupera memória, roda o agente e verifica cada módulo selecionado.",
      },
      {
        label: "Pedir decisão",
        detail:
          "Pausa de forma durável e espera uma aprovação ligada ao digest exato do artefato.",
      },
      {
        label: "Publicar sem surpresa",
        detail:
          "Reaplica e reverifica o bundle antes de criar ou reutilizar um draft PR determinístico.",
      },
      {
        label: "Fechar o ciclo",
        detail:
          "Grava um único resultado na memória e retorna referências duráveis para auditoria.",
      },
    ],
  },
  guide: {
    kicker: "Guia rápido",
    title: "Do trigger ao primeiro run.",
    intro:
      "O beta é um worker self-hosted. Codex é o primeiro harness; o protocolo foi desenhado para receber outros.",
    stepsLabel: "Passos do guia",
    steps: [
      { title: "Preparar", detail: "imagem + secrets" },
      { title: "Instalar", detail: "Helm + adapters" },
      { title: "Executar", detail: "trigger → workflow" },
      { title: "Aprovar", detail: "quando houver PR" },
    ],
    prepare: {
      eyebrow: "Pré-requisitos",
      title: "Prepare o terreno",
      text:
        "Go 1.27+ é necessário apenas para desenvolver o Pajé, que é implementado em Go. O repositório atendido pode usar qualquer linguagem; inclua o toolchain correspondente na imagem do worker. No beta, prepare Docker, Helm 3, Hatchet e autenticação do Codex.",
      requirements: [
        "Qualquer stack",
        "Docker",
        "Helm 3",
        "Hatchet",
        "Codex · primeiro harness",
      ],
    },
    install: {
      eyebrow: "Instalação",
      title: "Construa e instale",
      text:
        "Construa a imagem com uma revisão auditável e aplique o chart depois de configurar Secrets separados.",
      link: "Ver instalação completa e valores do Helm",
    },
    execute: {
      eyebrow: "Primeira execução",
      title: "Dispare um artifact run",
      beforeWorkflow: "Hoje, inicie",
      afterWorkflow: "no Hatchet. A integração pretendida levará esse trigger para hooks e skills do harness. Use",
      afterProfile: "com checks explícitos e o toolchain presente na imagem. O profile",
      afterGoProfile:
        "é só uma conveniência para descoberta de módulos e defaults de teste.",
      taskDescription: "Aumente o timeout do cliente e atualize os testes.",
      memoryQuery: "convenções do cliente HTTP",
    },
    approve: {
      eyebrow: "Publicação",
      title: "Aprove o artefato, não a intenção",
      beforeMode: "Para criar um draft PR, use",
      afterMode: "A fase de aprovação aguarda o evento",
      afterEvent:
        "com o mesmo run e digest. Se algo mudar, a decisão não é reaproveitada.",
      link: "Ver contrato do evento de aprovação",
    },
  },
  security: {
    kicker: "Segurança por fronteiras",
    titleLead: "O agente vê o necessário.",
    titleEnd: "O sistema guarda o sensível.",
    text:
      "Credenciais de Hatchet, Mem0 e GitHub não entram no ambiente do agente. Publicação acontece em um repositório bare novo e confiável, depois da verificação do código controlado pelo repositório.",
    link: "Ler garantias e conflitos",
    diagramLabel: "Fronteiras de credenciais",
    boundary: "worker boundary",
    credentials: "service credentials",
    runtime: "Agent runtime",
    selectedMemory: "memória selecionada",
    isolatedWorktree: "worktree isolado",
    allowedEnvironment: "ambiente permitido",
    workerTokens: "tokens do worker",
  },
  docs: {
    kicker: "Documentação",
    titleLead: "Entenda o contrato.",
    titleEnd: "Opere com confiança.",
    openReadme: "Abrir README completo",
    cards: [
      {
        eyebrow: "01 / Use",
        title: "Contrato do agente",
        text:
          "Envelope que hooks e skills enviam, com profiles, checks e modos de publicação do code-change@v1.",
      },
      {
        eyebrow: "02 / Operate",
        title: "Configuração",
        text:
          "Adapters, limites, ambiente filtrado e contratos das variáveis do worker.",
      },
      {
        eyebrow: "03 / Deploy",
        title: "Kubernetes + Helm",
        text: "Imagem, Secrets separados, persistência e valores para operar o beta.",
      },
      {
        eyebrow: "04 / Understand",
        title: "Design do beta",
        text:
          "Decisões, fronteiras e invariantes de segurança do fluxo de code change.",
      },
    ],
    betaScope: "scope",
    betaText:
      "Hoje, Pajé oferece o template tipado code-change@v1, o runner do Codex como primeiro harness, disparo pelo Hatchet, modo artifact ou draft PR no GitHub e uma única réplica. Hooks e skills agent-side e outros harnesses serão suportados no futuro. Merge automático, YAML arbitrário, releases e múltiplas réplicas ficam fora deste beta.",
  },
  closing: {
    kickerLead: "O agente pilota via hooks e skills.",
    kickerEnd: "Pajé torna o percurso durável.",
    titleLead: "Autonomia para qualquer stack.",
    titleEnd: "Codex primeiro, mais harnesses depois.",
    viewGitHub: "Ver projeto no GitHub",
    revisitGuide: "Rever o guia",
  },
  footer: {
    brandLabel: "Pajé — início",
    text: "Orquestração durável, pilotada pelo agente e independente da linguagem.",
  },
} satisfies SiteCatalog;

const english = {
  htmlLang: "en",
  metadata: {
    title: "Pajé — Durable orchestration piloted by the agent",
    description:
      "Designed for the agent to pilot through hooks and skills. Pajé makes changes durable for repositories in any language, with Codex as the first harness.",
    openGraphTitle: "Pajé — From request to pull request. Without losing the thread.",
    openGraphDescription:
      "Designed for the agent to pilot, repository-language-neutral, with Codex as the first harness.",
    openGraphLocale: "en_US",
    socialImageAlt: "Pajé social card",
  },
  languageSwitcher: {
    label: "Select language",
    english: "View in English",
    portuguese: "View in Brazilian Portuguese",
    spanish: "View in Spanish",
  },
  navigation: {
    brandLabel: "Pajé — home",
    primaryLabel: "Primary navigation",
    product: "Product",
    workflow: "How it works",
    guide: "Usage guide",
    docs: "Docs",
  },
  hero: {
    status: "Beta available · Codex is the first harness",
    kicker: "Durable orchestration piloted by the agent",
    titleLead: "From request to pull request.",
    titleAccent: " Without losing the thread.",
    lead:
      "Pajé was designed to be piloted by the agent itself through harness hooks and skills. It turns every change into a verifiable, repository-language-neutral workflow: the right context, isolated execution, human approval, and idempotent publication.",
    startGuide: "Start with the guide",
    readGitHub: "Read on GitHub",
    principlesLabel: "Principles and current support",
    principles: ["Agent-piloted design", "Hooks + skills", "Language-neutral", "Codex first"],
  },
  run: {
    ariaLabel: "Example Pajé run",
    stamp: "PERSISTED",
    state: "awaiting approval",
    completed: "completed",
    decisionNeeded: "decision needed",
    queued: "queued",
    artifact: "Verified artifact",
    files: "files",
    checks: "checks",
    secrets: "secrets",
    persisted: "state persisted",
  },
  proof: {
    ariaLabel: "Beta principles and status",
    items: [
      { value: "Agent", label: "model: hooks + skills" },
      { value: "Generic", label: "checks for any stack" },
      { value: "Codex", label: "first harness" },
      { value: "5", label: "durable phases" },
    ],
  },
  featuresHeading: {
    kicker: "The agent pilots. Pajé sustains.",
    titleLead: "Autonomy without",
    titleEnd: "losing the process.",
    text:
      "The harness preserves agent autonomy; Pajé provides a predictable contract for effects. Hooks and skills are the intended integration surface. Every action leaves evidence, and every retry knows where to continue.",
  },
  features: [
    {
      title: "Designed for the agent to pilot",
      text:
        "The product model puts harness hooks and skills at the entry point, so the agent itself can invoke the protocol, follow the run, and resume with context.",
      signal: "product direction",
    },
    {
      title: "Repository-language-neutral",
      text:
        "Workflows are repository-language-neutral. Pajé happens to be implemented in Go, but Go-native positioning is inconsequential; the generic profile runs structured checks for any stack available in the worker image.",
      signal: "any stack",
    },
    {
      title: "Durable execution",
      text:
        "Each phase persists its state. Retries resume the right work without relaunching the agent or duplicating effects.",
      signal: "restart-safe",
    },
    {
      title: "Scoped memory",
      text:
        "The agent receives relevant context by user and application. Worker credentials stay outside the execution.",
      signal: "Mem0 adapter",
    },
    {
      title: "Verifiable artifact",
      text:
        "Patch, output, verification, and preflight become an immutable bundle authenticated by SHA-256.",
      signal: "content-addressed",
    },
    {
      title: "Bound approval",
      text:
        "The human decision applies to one exact run and digest. If the artifact changes, the approval no longer applies.",
      signal: "artifact-bound",
    },
    {
      title: "Deterministic PR",
      text:
        "Branch, commit, and pull request are reused only when every binding matches. No force-push.",
      signal: "idempotent",
    },
    {
      title: "Replaceable harness",
      text:
        "Codex is the first supported harness. The execution boundary is designed to support other harnesses in the future without changing the durable protocol.",
      signal: "Codex first",
    },
  ],
  workflow: {
    titleLead: "A workflow that knows",
    titleEnd: "where it stands.",
    intro:
      "The agent starts and follows the work through the harness integration. Hatchet handles the queue, retries, and signals; Pajé owns the contract: state, context, artifacts, policy, and publication.",
    phases: [
      {
        label: "Establish the truth",
        detail:
          "Validates the input, reserves the idempotency key, and resolves the ref to an immutable commit.",
      },
      {
        label: "Build and prove",
        detail:
          "Prepares an isolated worktree, retrieves memory, runs the agent, and verifies every selected module.",
      },
      {
        label: "Request a decision",
        detail:
          "Pauses durably and waits for approval bound to the artifact's exact digest.",
      },
      {
        label: "Publish without surprises",
        detail:
          "Reapplies and reverifies the bundle before creating or reusing a deterministic draft PR.",
      },
      {
        label: "Close the loop",
        detail:
          "Writes one outcome to memory and returns durable references for auditing.",
      },
    ],
  },
  guide: {
    kicker: "Quick guide",
    title: "From trigger to first run.",
    intro:
      "The beta is a self-hosted worker. Codex is the first harness; the protocol was designed to support others in the future.",
    stepsLabel: "Guide steps",
    steps: [
      { title: "Prepare", detail: "image + secrets" },
      { title: "Install", detail: "Helm + adapters" },
      { title: "Run", detail: "trigger → workflow" },
      { title: "Approve", detail: "when a PR is needed" },
    ],
    prepare: {
      eyebrow: "Prerequisites",
      title: "Prepare the ground",
      text:
        "Go 1.27+ is required only to develop Pajé, which happens to be implemented in Go; Go-native positioning is inconsequential. The target repository can use any language: include its toolchain in the worker image. For the beta, prepare Docker, Helm 3, Hatchet, and Codex authentication.",
      requirements: [
        "Any stack",
        "Docker",
        "Helm 3",
        "Hatchet",
        "Codex · first harness",
      ],
    },
    install: {
      eyebrow: "Installation",
      title: "Build and install",
      text:
        "Build the image with an auditable revision and apply the chart after configuring separate Secrets.",
      link: "View the complete installation and Helm values",
    },
    execute: {
      eyebrow: "First run",
      title: "Start an artifact run",
      beforeWorkflow: "Today, start",
      afterWorkflow: "in Hatchet. The intended integration will move this trigger into harness hooks and skills. Use",
      afterProfile: "with explicit checks and the toolchain present in the image. The",
      afterGoProfile:
        "profile is only a convenience for module discovery and test defaults.",
      taskDescription: "Increase the client timeout and update the tests.",
      memoryQuery: "HTTP client conventions",
    },
    approve: {
      eyebrow: "Publication",
      title: "Approve the artifact, not the intent",
      beforeMode: "To create a draft PR, use",
      afterMode: "The approval phase waits for the event",
      afterEvent:
        "with the same run and digest. If anything changes, the decision is not reused.",
      link: "View the approval event contract",
    },
  },
  security: {
    kicker: "Security through boundaries",
    titleLead: "The agent sees what it needs.",
    titleEnd: "The system keeps sensitive data safe.",
    text:
      "Hatchet, Mem0, and GitHub credentials never enter the agent environment. Publication happens in a new trusted bare repository after repository-controlled code has been verified.",
    link: "Read guarantees and conflict behavior",
    diagramLabel: "Credential boundaries",
    boundary: "worker boundary",
    credentials: "service credentials",
    runtime: "Agent runtime",
    selectedMemory: "selected memory",
    isolatedWorktree: "isolated worktree",
    allowedEnvironment: "allowed environment",
    workerTokens: "worker tokens",
  },
  docs: {
    kicker: "Documentation",
    titleLead: "Understand the contract.",
    titleEnd: "Operate with confidence.",
    openReadme: "Open the complete README",
    cards: [
      {
        eyebrow: "01 / Use",
        title: "Agent contract",
        text:
          "The envelope sent by hooks and skills, including profiles, checks, and code-change@v1 publication modes.",
      },
      {
        eyebrow: "02 / Operate",
        title: "Configuration",
        text:
          "Adapters, limits, the filtered environment, and worker variable contracts.",
      },
      {
        eyebrow: "03 / Deploy",
        title: "Kubernetes + Helm",
        text:
          "Image, separate Secrets, persistence, and values for operating the beta.",
      },
      {
        eyebrow: "04 / Understand",
        title: "Beta design",
        text:
          "Decisions, boundaries, and security invariants for the code-change workflow.",
      },
    ],
    betaScope: "scope",
    betaText:
      "Today Pajé provides the typed code-change@v1 template, the Codex runner as the first harness, Hatchet triggering, artifact or GitHub draft PR mode, and one replica. Agent-side hooks and skills and other harnesses will be supported in the future. Automatic merge, arbitrary YAML, releases, and multiple replicas are outside this beta.",
  },
  closing: {
    kickerLead: "The agent pilots through hooks and skills.",
    kickerEnd: "Pajé makes the journey durable.",
    titleLead: "Autonomy for any stack.",
    titleEnd: "Codex first, more harnesses later.",
    viewGitHub: "View the project on GitHub",
    revisitGuide: "Revisit the guide",
  },
  footer: {
    brandLabel: "Pajé — home",
    text: "Durable orchestration, piloted by the agent and repository-language-neutral.",
  },
} satisfies SiteCatalog;

const spanish = {
  htmlLang: "es",
  metadata: {
    title: "Pajé — Orquestación duradera pilotada por el agente",
    description:
      "Diseñado para que el agente lo pilote mediante hooks y skills. Pajé hace duraderos los cambios en repositorios de cualquier lenguaje, con Codex como primer harness.",
    openGraphTitle: "Pajé — De la solicitud al pull request. Sin perder el hilo.",
    openGraphDescription:
      "Diseñado para que lo pilote el agente, neutral respecto al lenguaje del repositorio y con Codex como primer harness.",
    openGraphLocale: "es_ES",
    socialImageAlt: "Tarjeta social de Pajé",
  },
  languageSwitcher: {
    label: "Seleccionar idioma",
    english: "Ver en inglés",
    portuguese: "Ver en portugués de Brasil",
    spanish: "Ver en español",
  },
  navigation: {
    brandLabel: "Pajé — inicio",
    primaryLabel: "Navegación principal",
    product: "Producto",
    workflow: "Cómo funciona",
    guide: "Guía de uso",
    docs: "Docs",
  },
  hero: {
    status: "Beta disponible · Codex es el primer harness",
    kicker: "Orquestación duradera pilotada por el agente",
    titleLead: "De la solicitud al pull request.",
    titleAccent: " Sin perder el hilo.",
    lead:
      "Pajé fue diseñado para que el propio agente lo pilote mediante hooks y skills del harness. Convierte cada cambio en un workflow verificable y neutral respecto al lenguaje del repositorio: contexto adecuado, ejecución aislada, aprobación humana y publicación idempotente.",
    startGuide: "Empezar por la guía",
    readGitHub: "Leer en GitHub",
    principlesLabel: "Principios y soporte actual",
    principles: ["Diseño pilotado por el agente", "Hooks + skills", "Neutral al lenguaje", "Codex primero"],
  },
  run: {
    ariaLabel: "Ejemplo de ejecución de Pajé",
    stamp: "PERSISTIDO",
    state: "esperando aprobación",
    completed: "completado",
    decisionNeeded: "decisión necesaria",
    queued: "en cola",
    artifact: "Artefacto verificado",
    files: "archivos",
    checks: "checks",
    secrets: "secretos",
    persisted: "estado persistido",
  },
  proof: {
    ariaLabel: "Principios y estado de la beta",
    items: [
      { value: "Agente", label: "modelo: hooks + skills" },
      { value: "Generic", label: "checks para cualquier stack" },
      { value: "Codex", label: "primer harness" },
      { value: "5", label: "fases duraderas" },
    ],
  },
  featuresHeading: {
    kicker: "El agente pilota. Pajé sostiene.",
    titleLead: "Autonomía sin",
    titleEnd: "perder el proceso.",
    text:
      "El harness preserva la autonomía del agente; Pajé ofrece un contrato predecible para los efectos. Hooks y skills son la superficie de integración prevista. Cada acción deja evidencia y cada retry sabe desde dónde continuar.",
  },
  features: [
    {
      title: "Diseñado para que el agente pilote",
      text:
        "El modelo de producto coloca los hooks y skills del harness en la entrada para que el propio agente invoque el protocolo, siga el run y retome con contexto.",
      signal: "dirección del producto",
    },
    {
      title: "Neutral al lenguaje del repositorio",
      text:
        "Los workflows son neutrales respecto al lenguaje del repositorio. Pajé está implementado en Go, pero posicionarlo como Go-native es irrelevante; el profile generic ejecuta checks estructurados para cualquier stack disponible en la imagen del worker.",
      signal: "cualquier stack",
    },
    {
      title: "Ejecución duradera",
      text:
        "Cada fase persiste su estado. Los retries retoman el trabajo correcto sin volver a lanzar el agente ni duplicar efectos.",
      signal: "restart-safe",
    },
    {
      title: "Memoria con alcance",
      text:
        "El agente recibe contexto relevante por usuario y aplicación. Las credenciales del worker quedan fuera de la ejecución.",
      signal: "adaptador Mem0",
    },
    {
      title: "Artefacto verificable",
      text:
        "Patch, salida, verificaciones y preflight se convierten en un bundle inmutable autenticado por SHA-256.",
      signal: "content-addressed",
    },
    {
      title: "Aprobación vinculada",
      text:
        "La decisión humana vale para un run y un digest exactos. Si cambia el artefacto, la aprobación deja de ser válida.",
      signal: "artifact-bound",
    },
    {
      title: "PR determinista",
      text:
        "Branch, commit y pull request se reutilizan solo cuando coinciden todos los vínculos. Sin force-push.",
      signal: "idempotente",
    },
    {
      title: "Harness reemplazable",
      text:
        "Codex es el primer harness soportado. La frontera de ejecución está diseñada para incorporar otros harnesses en el futuro sin cambiar el protocolo duradero.",
      signal: "Codex primero",
    },
  ],
  workflow: {
    titleLead: "Un workflow que sabe",
    titleEnd: "dónde está.",
    intro:
      "El agente inicia y sigue el trabajo mediante la integración del harness. Hatchet gestiona la cola, los retries y las señales; Pajé se ocupa del contrato: estado, contexto, artefactos, política y publicación.",
    phases: [
      {
        label: "Fijar la verdad",
        detail:
          "Valida la entrada, reserva la clave de idempotencia y resuelve el ref a un commit inmutable.",
      },
      {
        label: "Hacer y demostrar",
        detail:
          "Prepara un worktree aislado, recupera memoria, ejecuta el agente y verifica cada módulo seleccionado.",
      },
      {
        label: "Solicitar una decisión",
        detail:
          "Se pausa de forma duradera y espera una aprobación vinculada al digest exacto del artefacto.",
      },
      {
        label: "Publicar sin sorpresas",
        detail:
          "Reaplica y vuelve a verificar el bundle antes de crear o reutilizar un draft PR determinista.",
      },
      {
        label: "Cerrar el ciclo",
        detail:
          "Guarda un único resultado en la memoria y devuelve referencias duraderas para auditoría.",
      },
    ],
  },
  guide: {
    kicker: "Guía rápida",
    title: "Del trigger al primer run.",
    intro:
      "La beta es un worker self-hosted. Codex es el primer harness; el protocolo fue diseñado para incorporar otros en el futuro.",
    stepsLabel: "Pasos de la guía",
    steps: [
      { title: "Preparar", detail: "imagen + secrets" },
      { title: "Instalar", detail: "Helm + adapters" },
      { title: "Ejecutar", detail: "trigger → workflow" },
      { title: "Aprobar", detail: "cuando se necesite un PR" },
    ],
    prepare: {
      eyebrow: "Requisitos previos",
      title: "Prepara el terreno",
      text:
        "Go 1.27+ solo es necesario para desarrollar Pajé, que está implementado en Go; posicionarlo como Go-native es irrelevante. El repositorio atendido puede usar cualquier lenguaje: incluye su toolchain en la imagen del worker. Para la beta, prepara Docker, Helm 3, Hatchet y la autenticación de Codex.",
      requirements: [
        "Cualquier stack",
        "Docker",
        "Helm 3",
        "Hatchet",
        "Codex · primer harness",
      ],
    },
    install: {
      eyebrow: "Instalación",
      title: "Construye e instala",
      text:
        "Construye la imagen con una revisión auditable y aplica el chart después de configurar Secrets separados.",
      link: "Ver la instalación completa y los valores de Helm",
    },
    execute: {
      eyebrow: "Primera ejecución",
      title: "Inicia un artifact run",
      beforeWorkflow: "Hoy, inicia",
      afterWorkflow: "en Hatchet. La integración prevista llevará este trigger a los hooks y skills del harness. Usa",
      afterProfile: "con checks explícitos y el toolchain presente en la imagen. El profile",
      afterGoProfile:
        "solo facilita el descubrimiento de módulos y los defaults de pruebas.",
      taskDescription: "Aumenta el timeout del cliente y actualiza las pruebas.",
      memoryQuery: "convenciones del cliente HTTP",
    },
    approve: {
      eyebrow: "Publicación",
      title: "Aprueba el artefacto, no la intención",
      beforeMode: "Para crear un draft PR, usa",
      afterMode: "La fase de aprobación espera el evento",
      afterEvent:
        "con el mismo run y digest. Si algo cambia, la decisión no se reutiliza.",
      link: "Ver el contrato del evento de aprobación",
    },
  },
  security: {
    kicker: "Seguridad mediante fronteras",
    titleLead: "El agente ve lo necesario.",
    titleEnd: "El sistema protege lo sensible.",
    text:
      "Las credenciales de Hatchet, Mem0 y GitHub no entran en el entorno del agente. La publicación ocurre en un repositorio bare nuevo y confiable después de verificar el código controlado por el repositorio.",
    link: "Leer garantías y comportamiento ante conflictos",
    diagramLabel: "Fronteras de credenciales",
    boundary: "frontera del worker",
    credentials: "credenciales del servicio",
    runtime: "Runtime del agente",
    selectedMemory: "memoria seleccionada",
    isolatedWorktree: "worktree aislado",
    allowedEnvironment: "entorno permitido",
    workerTokens: "tokens del worker",
  },
  docs: {
    kicker: "Documentación",
    titleLead: "Entiende el contrato.",
    titleEnd: "Opera con confianza.",
    openReadme: "Abrir el README completo",
    cards: [
      {
        eyebrow: "01 / Usar",
        title: "Contrato del agente",
        text:
          "El envelope que envían hooks y skills, con profiles, checks y modos de publicación de code-change@v1.",
      },
      {
        eyebrow: "02 / Operar",
        title: "Configuración",
        text:
          "Adapters, límites, entorno filtrado y contratos de las variables del worker.",
      },
      {
        eyebrow: "03 / Desplegar",
        title: "Kubernetes + Helm",
        text:
          "Imagen, Secrets separados, persistencia y valores para operar la beta.",
      },
      {
        eyebrow: "04 / Entender",
        title: "Diseño de la beta",
        text:
          "Decisiones, fronteras e invariantes de seguridad del workflow de code change.",
      },
    ],
    betaScope: "alcance",
    betaText:
      "Hoy Pajé ofrece el template tipado code-change@v1, el runner de Codex como primer harness, trigger mediante Hatchet, modo artifact o draft PR en GitHub y una única réplica. Los hooks y skills agent-side y otros harnesses serán soportados en el futuro. Merge automático, YAML arbitrario, releases y múltiples réplicas quedan fuera de esta beta.",
  },
  closing: {
    kickerLead: "El agente pilota mediante hooks y skills.",
    kickerEnd: "Pajé hace duradero el recorrido.",
    titleLead: "Autonomía para cualquier stack.",
    titleEnd: "Codex primero, más harnesses después.",
    viewGitHub: "Ver el proyecto en GitHub",
    revisitGuide: "Volver a la guía",
  },
  footer: {
    brandLabel: "Pajé — inicio",
    text:
      "Orquestación duradera, pilotada por el agente y neutral respecto al lenguaje del repositorio.",
  },
} satisfies SiteCatalog;

export const catalogs: Record<Locale, SiteCatalog> = {
  en: english,
  "pt-br": portuguese,
  es: spanish,
};

export function isLocale(value: string): value is Locale {
  return locales.includes(value as Locale);
}

export function localeFromRequestHeader(value: string | null): Locale {
  return isLocale(value ?? "") ? (value as Locale) : "en";
}

const expectedCollectionLengths = {
  features: 8,
  phases: 5,
  guideSteps: 4,
  docs: 4,
} as const;

function assertCompleteCopy(value: unknown, path: string): void {
  if (typeof value === "string") {
    if (value.trim() === "") {
      throw new Error(`Empty catalog value: ${path}`);
    }
    return;
  }

  if (Array.isArray(value)) {
    value.forEach((item, index) => assertCompleteCopy(item, `${path}[${index}]`));
    return;
  }

  if (value && typeof value === "object") {
    for (const [key, child] of Object.entries(value)) {
      assertCompleteCopy(child, `${path}.${key}`);
    }
  }
}

for (const [locale, catalog] of Object.entries(catalogs)) {
  assertCompleteCopy(catalog, locale);

  const lengths = {
    features: catalog.features.length,
    phases: catalog.workflow.phases.length,
    guideSteps: catalog.guide.steps.length,
    docs: catalog.docs.cards.length,
  };

  for (const [collection, expected] of Object.entries(expectedCollectionLengths)) {
    if (lengths[collection as keyof typeof lengths] !== expected) {
      throw new Error(`Incomplete ${locale} catalog: ${collection}`);
    }
  }
}
