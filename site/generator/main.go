package main

import (
	"bytes"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/araihu/goshtoso/assets"
	"github.com/araihu/goshtoso/components/badge"
	"github.com/araihu/goshtoso/components/head"
)

type pageCopy struct {
	Locale               string
	Lang                 string
	Title                string
	Description          string
	SkipLabel            string
	HomeLabel            string
	PrimaryLabel         string
	ProtocolLabel        string
	BoundaryLabel        string
	LanguageLabel        string
	Eyebrow              string
	Hero                 string
	Lead                 string
	Guide                string
	Protocol             string
	ProtocolBody         string
	RunLabel             string
	BoundarySectionLabel string
	Boundary             string
	BoundaryBody         string
	BetaSectionLabel     string
	Current              string
	CurrentBody          string
	GitHub               string
	FooterLine           string
}

var catalog = []pageCopy{
	{
		Locale: "en", Lang: "en", Title: "Pajé — Durable orchestration piloted by the agent",
		Description:          "Durable orchestration for agent-piloted code changes, with Codex as the first harness.",
		SkipLabel:            "Skip to content",
		HomeLabel:            "Pajé home",
		PrimaryLabel:         "Primary",
		ProtocolLabel:        "Protocol",
		BoundaryLabel:        "Boundary",
		LanguageLabel:        "Language",
		Eyebrow:              "DURABLE AGENT ORCHESTRATION",
		Hero:                 "From request to pull request. Keep the thread.",
		Lead:                 "Pajé gives an agent a durable protocol for code change: isolate execution, verify work, wait for human approval when needed, then publish idempotently.",
		Guide:                "Read the workflow guide",
		Protocol:             "A run is a record, not a chat transcript.",
		ProtocolBody:         "Resolve locks an immutable revision and scoped context. Execute creates evidence. Approval binds a person to the verified artifact. Publish and finalize can safely resume after interruption.",
		RunLabel:             "Illustrative durable run record",
		BoundarySectionLabel: "Execution boundary",
		Boundary:             "Agent autonomy, bounded effects.",
		BoundaryBody:         "Hooks and skills are the intended agent surface. Pajé owns durable state, artifact identity, verification, approval, and publication behind provider-neutral Go ports.",
		BetaSectionLabel:     "Beta scope",
		Current:              "Current beta boundary.",
		CurrentBody:          "The built-in code-change@v1 template runs through Hatchet. Artifact mode is default. Pull-request mode adds artifact-bound approval and idempotent draft GitHub publication. Codex is the first supported harness.",
		GitHub:               "Open Pajé on GitHub",
		FooterLine:           "Pajé · durable workflow infrastructure",
	},
	{
		Locale: "pt-br", Lang: "pt-BR", Title: "Pajé — Orquestração durável pilotada pelo agente",
		Description:          "Orquestração durável para mudanças de código pilotadas pelo agente, com Codex como primeiro harness.",
		SkipLabel:            "Pular para o conteúdo",
		HomeLabel:            "Início do Pajé",
		PrimaryLabel:         "Principal",
		ProtocolLabel:        "Protocolo",
		BoundaryLabel:        "Limites",
		LanguageLabel:        "Idioma",
		Eyebrow:              "ORQUESTRAÇÃO DURÁVEL PILOTADA PELO AGENTE",
		Hero:                 "Do pedido ao pull request. Sem perder o fio.",
		Lead:                 "Pajé dá ao agente um protocolo durável para mudanças de código: isola a execução, verifica o trabalho, aguarda aprovação humana quando necessário e publica de forma idempotente.",
		Guide:                "Ler o guia do workflow",
		Protocol:             "Um run é um registro, não uma transcrição de chat.",
		ProtocolBody:         "Resolve fixa uma revisão imutável e contexto escopado. Execute produz evidência. Approval vincula uma pessoa ao artefato verificado. Publish e finalize retomam com segurança após interrupções.",
		RunLabel:             "Registro ilustrativo de um run durável",
		BoundarySectionLabel: "Limite de execução",
		Boundary:             "Autonomia do agente, efeitos delimitados.",
		BoundaryBody:         "Hooks e skills são a superfície esperada para o agente. Pajé mantém estado durável, identidade do artefato, verificação, aprovação e publicação atrás de portas Go neutras ao provedor.",
		BetaSectionLabel:     "Escopo do beta",
		Current:              "Limite atual do beta.",
		CurrentBody:          "O template embutido code-change@v1 roda pelo Hatchet. O modo artifact é padrão. O modo pull request adiciona aprovação vinculada ao artefato e publicação idempotente de rascunho no GitHub. Codex é o primeiro harness suportado.",
		GitHub:               "Abrir Pajé no GitHub",
		FooterLine:           "Pajé · infraestrutura de workflows duráveis",
	},
	{
		Locale: "es", Lang: "es", Title: "Pajé — Orquestación duradera pilotada por el agente",
		Description:          "Orquestación duradera para cambios de código pilotados por el agente, con Codex como primer harness.",
		SkipLabel:            "Saltar al contenido",
		HomeLabel:            "Inicio de Pajé",
		PrimaryLabel:         "Principal",
		ProtocolLabel:        "Protocolo",
		BoundaryLabel:        "Límites",
		LanguageLabel:        "Idioma",
		Eyebrow:              "ORQUESTACIÓN DURADERA PILOTADA POR EL AGENTE",
		Hero:                 "De la solicitud al pull request. Sin perder el hilo.",
		Lead:                 "Pajé da al agente un protocolo duradero para cambios de código: aísla la ejecución, verifica el trabajo, espera aprobación humana cuando hace falta y publica de forma idempotente.",
		Guide:                "Leer la guía del workflow",
		Protocol:             "Una ejecución es un registro, no una transcripción de chat.",
		ProtocolBody:         "Resolve fija una revisión inmutable y contexto acotado. Execute produce evidencia. Approval vincula una persona al artefacto verificado. Publish y finalize pueden reanudarse con seguridad tras una interrupción.",
		RunLabel:             "Registro ilustrativo de una ejecución duradera",
		BoundarySectionLabel: "Límite de ejecución",
		Boundary:             "Autonomía del agente, efectos acotados.",
		BoundaryBody:         "Hooks y skills son la superficie prevista para el agente. Pajé gestiona estado duradero, identidad del artefacto, verificación, aprobación y publicación detrás de puertos Go neutrales al proveedor.",
		BetaSectionLabel:     "Alcance de la beta",
		Current:              "Límite actual de la beta.",
		CurrentBody:          "La plantilla integrada code-change@v1 se ejecuta mediante Hatchet. El modo artifact es el predeterminado. El modo pull request añade aprobación ligada al artefacto y publicación idempotente de borrador en GitHub. Codex es el primer harness compatible.",
		GitHub:               "Abrir Pajé en GitHub",
		FooterLine:           "Pajé · infraestructura de workflows duraderos",
	},
}

var assetURL = regexp.MustCompile(`(?:href|src)="(/assets/[^"]+)"`)

//go:embed site.css
var siteCSS []byte

var documentTemplate = template.Must(template.New("document").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}} · Pajé</title><meta name="description" content="{{.Description}}"><meta property="og:title" content="{{.Title}}"><meta property="og:description" content="{{.Description}}"><meta property="og:image" content="https://paje.araihu.com/og.png"><meta name="twitter:card" content="summary_large_image"><link rel="canonical" href="https://paje.araihu.com/{{.Locale}}"><link rel="alternate" hreflang="en" href="https://paje.araihu.com/en"><link rel="alternate" hreflang="pt-BR" href="https://paje.araihu.com/pt-br"><link rel="alternate" hreflang="es" href="https://paje.araihu.com/es"><link rel="alternate" hreflang="x-default" href="https://paje.araihu.com/en"><link rel="icon" href="/paje-icon-background.svg">{{.Dependencies}}<link rel="stylesheet" href="/site.css"></head>
<body><a class="skip" href="#main">{{.SkipLabel}}</a><header class="mast"><a class="brand" href="/{{.Locale}}" aria-label="{{.HomeLabel}}"><img src="/paje-logo-transparent.svg" alt="" width="166" height="41"></a><nav aria-label="{{.PrimaryLabel}}"><a href="#protocol">{{.ProtocolLabel}}</a><a href="#boundary">{{.BoundaryLabel}}</a><a href="https://github.com/araihu/paje">GitHub</a></nav><div class="languages" aria-label="{{.LanguageLabel}}"><a href="/en"{{if eq .Locale "en"}} aria-current="page"{{end}}>EN</a><a href="/pt-br"{{if eq .Locale "pt-br"}} aria-current="page"{{end}}>PT</a><a href="/es"{{if eq .Locale "es"}} aria-current="page"{{end}}>ES</a></div></header>
<main id="main"><section class="hero"><div class="hero-copy"><div class="status">{{.Status}}</div><p class="eyebrow">{{.Eyebrow}}</p><h1>{{.Hero}}</h1><p class="lead">{{.Lead}}</p><div class="actions"><a class="action" href="#protocol">{{.Guide}} <span aria-hidden="true">↘</span></a><a class="quiet-link" href="https://github.com/araihu/paje">{{.GitHub}}</a></div></div><aside class="run" aria-label="{{.RunLabel}}"><div class="run-head"><span>code-change@v1</span><span>persisted</span></div><ol><li class="done"><b>resolve</b><span>revision + context</span></li><li class="done"><b>execute</b><span>isolated evidence</span></li><li class="active"><b>approval</b><span>artifact bound</span></li><li><b>publish</b><span>idempotent</span></li><li><b>finalize</b><span>durable outcome</span></li></ol><footer><span>artifact mode</span><span>read-only example</span></footer></aside></section>
<section id="protocol" class="story"><p class="section-label">{{.ProtocolLabel}}</p><h2>{{.Protocol}}</h2><p>{{.ProtocolBody}}</p></section>
<section id="boundary" class="split"><article><p class="section-label">{{.BoundarySectionLabel}}</p><h2>{{.Boundary}}</h2><p>{{.BoundaryBody}}</p></article><article><p class="section-label">{{.BetaSectionLabel}}</p><h2>{{.Current}}</h2><p>{{.CurrentBody}}</p></article></section>
</main><footer class="foot"><img src="/paje-icon-background.svg" alt="" width="26" height="26"><span>{{.FooterLine}}</span><a href="https://github.com/araihu/paje">{{.GitHub}}</a></footer></body></html>`))

func main() {
	out := flag.String("out", "../public", "static site output directory")
	flag.Parse()
	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(out string) error {
	for _, name := range []string{"en", "pt-br", "es", "assets", "site.css"} {
		if err := os.RemoveAll(filepath.Join(out, name)); err != nil {
			return err
		}
	}
	dependencies, err := render(head.DependenciesMinimal(
		head.WithLocalRuntime(),
		head.WithoutDependency(head.DependencyAlpineJS),
		head.WithoutDependency(head.DependencyHTMX),
	))
	if err != nil {
		return err
	}
	if err := copyGoshtosoAssets(out, dependencies); err != nil {
		return err
	}
	if err := write(filepath.Join(out, "site.css"), siteCSS); err != nil {
		return err
	}
	for _, page := range catalog {
		document, err := renderDocument(page, dependencies)
		if err != nil {
			return err
		}
		if err := write(filepath.Join(out, page.Locale, "index.html"), document); err != nil {
			return err
		}
	}
	return nil
}

func renderDocument(page pageCopy, dependencies string) ([]byte, error) {
	status, err := render(badge.Badge(badge.Config{Label: "BETA · CODEX FIRST", Tone: badge.ToneInfo, Appearance: badge.AppearanceSoft, Size: badge.SizeSM, Indicator: true}))
	if err != nil {
		return nil, err
	}
	data := struct {
		pageCopy
		Dependencies template.HTML
		Status       template.HTML
	}{
		pageCopy:     page,
		Dependencies: template.HTML(dependencies), // Trusted Goshtoso component output.
		Status:       template.HTML(status),       // Trusted Goshtoso component output.
	}
	var output bytes.Buffer
	if err := documentTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type component interface {
	Render(context.Context, io.Writer) error
}

func render(value component) (string, error) {
	var output bytes.Buffer
	if err := value.Render(context.Background(), &output); err != nil {
		return "", err
	}
	return output.String(), nil
}

func copyGoshtosoAssets(out, dependencies string) error {
	handler := assets.Handler()
	seen := map[string]bool{}
	for _, match := range assetURL.FindAllStringSubmatch(dependencies, -1) {
		assetPath := match[1]
		if seen[assetPath] {
			continue
		}
		seen[assetPath] = true
		request := httptest.NewRequest(http.MethodGet, "https://static.invalid"+assetPath, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			return fmt.Errorf("copy Goshtoso asset %s: status %d", assetPath, response.Code)
		}
		if err := write(filepath.Join(out, strings.TrimPrefix(assetPath, "/")), response.Body.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
