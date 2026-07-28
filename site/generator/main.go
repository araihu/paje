package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html"
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

type copy struct {
	locale       string
	lang         string
	title        string
	description  string
	hero         string
	lead         string
	guide        string
	protocol     string
	protocolBody string
	boundary     string
	boundaryBody string
	current      string
	currentBody  string
	github       string
}

var catalog = []copy{
	{
		locale: "en", lang: "en", title: "Pajé — Durable orchestration piloted by the agent",
		description: "Durable orchestration for agent-piloted code changes, with Codex as the first harness.",
		hero:        "From request to pull request. Keep the thread.",
		lead:        "Pajé gives an agent a durable protocol for code change: isolate execution, verify work, wait for human approval when needed, then publish idempotently.",
		guide:       "Read the workflow guide", protocol: "A run is a record, not a chat transcript.",
		protocolBody: "Resolve locks an immutable revision and scoped context. Execute creates evidence. Approval binds a person to the verified artifact. Publish and finalize can safely resume after interruption.",
		boundary:     "Agent autonomy, bounded effects.",
		boundaryBody: "Hooks and skills are the intended agent surface. Pajé owns durable state, artifact identity, verification, approval, and publication behind provider-neutral Go ports.",
		current:      "Current beta boundary.",
		currentBody:  "The built-in code-change@v1 template runs through Hatchet. Artifact mode is default. Pull-request mode adds artifact-bound approval and idempotent draft GitHub publication. Codex is the first supported harness.",
		github:       "Open Pajé on GitHub",
	},
	{
		locale: "pt-br", lang: "pt-BR", title: "Pajé — Orquestração durável pilotada pelo agente",
		description: "Orquestração durável para mudanças de código pilotadas pelo agente, com Codex como primeiro harness.",
		hero:        "Do pedido ao pull request. Sem perder o fio.",
		lead:        "Pajé dá ao agente um protocolo durável para mudanças de código: isola a execução, verifica o trabalho, aguarda aprovação humana quando necessário e publica de forma idempotente.",
		guide:       "Ler o guia do workflow", protocol: "Um run é um registro, não uma transcrição de chat.",
		protocolBody: "Resolve fixa uma revisão imutável e contexto escopado. Execute produz evidência. Approval vincula uma pessoa ao artefato verificado. Publish e finalize retomam com segurança após interrupções.",
		boundary:     "Autonomia do agente, efeitos delimitados.",
		boundaryBody: "Hooks e skills são a superfície esperada para o agente. Pajé mantém estado durável, identidade do artefato, verificação, aprovação e publicação atrás de portas Go neutras ao provedor.",
		current:      "Limite atual do beta.",
		currentBody:  "O template embutido code-change@v1 roda pelo Hatchet. O modo artifact é padrão. O modo pull request adiciona aprovação vinculada ao artefato e publicação idempotente de rascunho no GitHub. Codex é o primeiro harness suportado.",
		github:       "Abrir Pajé no GitHub",
	},
	{
		locale: "es", lang: "es", title: "Pajé — Orquestación duradera pilotada por el agente",
		description: "Orquestación duradera para cambios de código pilotados por el agente, con Codex como primer harness.",
		hero:        "De la solicitud al pull request. Sin perder el hilo.",
		lead:        "Pajé da al agente un protocolo duradero para cambios de código: aísla la ejecución, verifica el trabajo, espera aprobación humana cuando hace falta y publica de forma idempotente.",
		guide:       "Leer la guía del workflow", protocol: "Una ejecución es un registro, no una transcripción de chat.",
		protocolBody: "Resolve fija una revisión inmutable y contexto acotado. Execute produce evidencia. Approval vincula una persona al artefacto verificado. Publish y finalize pueden reanudarse con seguridad tras una interrupción.",
		boundary:     "Autonomía del agente, efectos acotados.",
		boundaryBody: "Hooks y skills son la superficie prevista para el agente. Pajé gestiona estado duradero, identidad del artefacto, verificación, aprobación y publicación detrás de puertos Go neutrales al proveedor.",
		current:      "Límite actual de la beta.",
		currentBody:  "La plantilla integrada code-change@v1 se ejecuta mediante Hatchet. El modo artifact es el predeterminado. El modo pull request añade aprobación ligada al artefacto y publicación idempotente de borrador en GitHub. Codex es el primer harness compatible.",
		github:       "Abrir Pajé en GitHub",
	},
}

var assetURL = regexp.MustCompile(`(?:href|src)="(/assets/[^"]+)"`)

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
	deps, err := render(head.DependenciesMinimal(head.WithLocalRuntime()))
	if err != nil {
		return err
	}
	if err := copyGoshtosoAssets(out, deps); err != nil {
		return err
	}
	if err := write(filepath.Join(out, "site.css"), []byte(style)); err != nil {
		return err
	}
	for _, c := range catalog {
		page, err := document(c, deps)
		if err != nil {
			return err
		}
		if err := write(filepath.Join(out, c.locale, "index.html"), []byte(page)); err != nil {
			return err
		}
	}
	return nil
}

func document(c copy, deps string) (string, error) {
	status, err := render(badge.Badge(badge.Config{Label: "BETA · CODEX FIRST", Tone: badge.ToneInfo, Appearance: badge.AppearanceSoft, Size: badge.SizeSM, Indicator: true}))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="%s"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="description" content="%s"><meta property="og:title" content="%s"><meta property="og:description" content="%s"><meta property="og:image" content="/og.png"><meta name="twitter:card" content="summary_large_image"><link rel="canonical" href="/%s"><link rel="icon" href="/paje-favicon.svg">%s<link rel="stylesheet" href="/site.css"></head>
<body><a class="skip" href="#main">Skip to content</a><header class="mast"><a class="brand" href="/%s" aria-label="Pajé home"><img src="/paje-mark.svg" alt="" width="34" height="34"><span>Pajé</span></a><nav aria-label="Primary"><a href="#protocol">Protocol</a><a href="#boundary">Boundary</a><a href="https://github.com/araihu/paje">GitHub</a></nav><div class="languages" aria-label="Language"><a href="/en"%s>EN</a><a href="/pt-br"%s>PT</a><a href="/es"%s>ES</a></div></header>
<main id="main"><section class="hero"><div class="hero-copy"><div class="status">%s</div><p class="eyebrow">DURABLE AGENT ORCHESTRATION</p><h1>%s</h1><p class="lead">%s</p><div class="actions"><a class="action" href="#protocol">%s <span aria-hidden="true">↘</span></a><a class="quiet-link" href="https://github.com/araihu/paje">%s</a></div></div><aside class="run" aria-label="Illustrative durable run record"><div class="run-head"><span>code-change@v1</span><span>persisted</span></div><ol><li class="done"><b>resolve</b><span>revision + context</span></li><li class="done"><b>execute</b><span>isolated evidence</span></li><li class="active"><b>approval</b><span>artifact bound</span></li><li><b>publish</b><span>idempotent</span></li><li><b>finalize</b><span>durable outcome</span></li></ol><footer><span>artifact mode</span><span>read-only example</span></footer></aside></section>
<section id="protocol" class="story"><p class="section-label">Protocol</p><h2>%s</h2><p>%s</p></section>
<section id="boundary" class="split"><article><p class="section-label">Execution boundary</p><h2>%s</h2><p>%s</p></article><article><p class="section-label">Beta scope</p><h2>%s</h2><p>%s</p></article></section>
</main><footer class="foot"><img src="/paje-mark-reverse.svg" alt="" width="26" height="26"><span>Pajé · durable workflow infrastructure</span><a href="https://github.com/araihu/paje">%s</a></footer></body></html>`,
		html.EscapeString(c.lang), html.EscapeString(c.description), html.EscapeString(c.title), html.EscapeString(c.description), c.locale, deps, c.locale,
		selected(c.locale, "en"), selected(c.locale, "pt-br"), selected(c.locale, "es"), status, html.EscapeString(c.hero), html.EscapeString(c.lead), html.EscapeString(c.guide), html.EscapeString(c.github), html.EscapeString(c.protocol), html.EscapeString(c.protocolBody), html.EscapeString(c.boundary), html.EscapeString(c.boundaryBody), html.EscapeString(c.current), html.EscapeString(c.currentBody), html.EscapeString(c.github)), nil
}

func selected(current, locale string) string {
	if current == locale {
		return ` aria-current="page"`
	}
	return ""
}

type component interface {
	Render(context.Context, io.Writer) error
}

func render(c component) (string, error) {
	var b bytes.Buffer
	if err := c.Render(context.Background(), &b); err != nil {
		return "", err
	}
	return b.String(), nil
}

func copyGoshtosoAssets(out, deps string) error {
	h := assets.Handler()
	seen := map[string]bool{}
	for _, match := range assetURL.FindAllStringSubmatch(deps, -1) {
		path := match[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		req := httptest.NewRequest(http.MethodGet, "https://static.invalid"+path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			return fmt.Errorf("copy Goshtoso asset %s: status %d", path, rr.Code)
		}
		if err := write(filepath.Join(out, strings.TrimPrefix(path, "/")), rr.Body.Bytes()); err != nil {
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

const style = `:root{--paper:#f2efe7;--paper-deep:#e8e2d6;--ink:#131921;--muted:#62686d;--line:#cbc6bb;--blue:#2855f5;--night:#101722;--green:#2c8055}*{box-sizing:border-box}html{scroll-behavior:smooth}body{margin:0;background:var(--paper);color:var(--ink);font-family:ui-sans-serif,system-ui,sans-serif;-webkit-font-smoothing:antialiased}a{color:inherit}.skip{position:absolute;left:1rem;top:-4rem;background:var(--blue);color:#fff;padding:.8rem 1rem;z-index:20}.skip:focus{top:1rem}.mast{width:min(1440px,calc(100% - 4rem));min-height:82px;margin:auto;border-bottom:1px solid var(--line);display:grid;grid-template-columns:1fr auto 1fr;align-items:center;gap:1rem}.brand{display:inline-flex;align-items:center;gap:.65rem;text-decoration:none;font-size:1.25rem;font-weight:780;letter-spacing:-.04em}.mast nav{display:flex;gap:2rem;font-size:.9rem}.mast nav a,.quiet-link{color:#4d5359;text-underline-offset:.3em}.mast nav a:hover,.quiet-link:hover{color:var(--blue)}.languages{justify-self:end;display:flex;border:1px solid var(--line);font-size:.72rem;font-weight:750;letter-spacing:.08em}.languages a{padding:.5rem .6rem;text-decoration:none}.languages a[aria-current]{background:var(--ink);color:#fff}.hero{width:min(1240px,calc(100% - 4rem));min-height:720px;margin:auto;display:grid;grid-template-columns:1.1fr .9fr;gap:clamp(3rem,8vw,9rem);align-items:center}.status{margin-bottom:2rem}.eyebrow,.section-label{margin:0 0 1rem;color:var(--blue);font-size:.72rem;font-weight:800;letter-spacing:.11em}.hero h1{max-width:760px;margin:0;font-size:clamp(3.75rem,7vw,6.4rem);line-height:.94;letter-spacing:-.04em}.lead{max-width:620px;margin:2rem 0 0;font-size:clamp(1.1rem,1.5vw,1.35rem);line-height:1.55;color:#4d5359}.actions{display:flex;align-items:center;gap:1.5rem;margin-top:2.25rem}.action{display:inline-flex;gap:1.3rem;align-items:center;background:var(--blue);color:#fff;padding:1rem 1.15rem;text-decoration:none;font-size:.85rem;font-weight:750;box-shadow:7px 7px 0 rgba(19,25,33,.16)}.action:hover{transform:translate(2px,2px);box-shadow:4px 4px 0 rgba(19,25,33,.18)}.run{background:#fcfbf7;border:1px solid #c8c3b8;box-shadow:18px 18px 0 var(--paper-deep);transform:rotate(1deg)}.run-head,.run footer{padding:1.2rem 1.4rem;display:flex;justify-content:space-between;font-size:.68rem;letter-spacing:.09em;text-transform:uppercase;color:#74797c}.run-head{border-bottom:1px solid #d9d5cc;color:var(--ink);font-weight:700}.run ol{list-style:none;margin:0;padding:.5rem 1.4rem}.run li{display:grid;grid-template-columns:1fr auto;gap:1rem;padding:1rem 0;border-bottom:1px solid #ebe8e1;color:#878b8d}.run li:last-child{border:0}.run li b{font-size:.95rem}.run li span{font-size:.72rem}.run li.done b{color:var(--green)}.run li.active b{color:var(--blue)}.run footer{border-top:1px solid #d9d5cc}.story,.split{padding:8rem max(2rem,calc((100vw - 1240px)/2));border-top:1px solid var(--line)}.story h2,.split h2{max-width:900px;margin:0;font-size:clamp(2.6rem,5vw,4.8rem);line-height:.99;letter-spacing:-.04em}.story>p:last-child,.split article>p:last-child{max-width:67ch;margin:2rem 0 0;color:var(--muted);font-size:1.1rem;line-height:1.7}.split{display:grid;grid-template-columns:1fr 1fr;gap:clamp(3rem,10vw,12rem);background:var(--night);color:#fff}.split .section-label{color:#99aefc}.split article>p:last-child{color:#c3cbd7}.foot{padding:2rem max(2rem,calc((100vw - 1240px)/2));background:var(--night);color:#fff;display:flex;align-items:center;gap:.8rem;border-top:1px solid #303947;font-size:.85rem}.foot a{margin-left:auto;color:#99aefc}@media(max-width:760px){.mast{width:calc(100% - 2rem);grid-template-columns:1fr auto;min-height:72px}.mast nav{display:none}.hero{width:calc(100% - 2rem);grid-template-columns:1fr;padding:5rem 0;min-height:0}.hero h1{font-size:clamp(3.4rem,16vw,5.2rem)}.run{margin:1rem 0;transform:none}.story,.split{padding:5rem 1rem}.split{grid-template-columns:1fr}.foot{padding:1.5rem 1rem;flex-wrap:wrap}.foot a{margin-left:0;width:100%}}@media(prefers-reduced-motion:no-preference){.run{transition:transform .35s cubic-bezier(.2,.8,.2,1)}.run:hover{transform:rotate(0)}}`
