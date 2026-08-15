package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	glowbomRepositoryURL  = "https://github.com/glowbom/glowbom-oss"
	glowbomWebsiteURL     = "https://glowbom.com"
	codexAppServerURL     = "https://developers.openai.com/codex/app-server/"
	glowbomDesktopPDFPath = "docs/2026/Glowbom_Desktop_A_Sketch_to_Software_System.pdf"
)

type backendInfo struct {
	Name                   string            `json:"name"`
	Summary                string            `json:"summary"`
	HowItWorks             string            `json:"howItWorks"`
	RuntimeURL             string            `json:"runtimeURL"`
	Links                  []backendInfoLink `json:"links"`
	Drivers                []backendDriver   `json:"drivers"`
	ProjectDescriptionURL  string            `json:"projectDescriptionURL,omitempty"`
	ProjectDescriptionPath string            `json:"projectDescriptionPath,omitempty"`
}

type backendInfoLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type backendDriver struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Status      string `json:"status"` // available | planned
	Description string `json:"description"`
}

func resolveGlowbomDesktopPDFPath() (string, bool) {
	candidates := []string{
		filepath.FromSlash(filepath.Join("..", glowbomDesktopPDFPath)),
		filepath.FromSlash(glowbomDesktopPDFPath),
	}

	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		return abs, true
	}

	return "", false
}

func resolveGlowbomPublicAssetPath(fileName string) (string, bool) {
	safeName := strings.TrimSpace(fileName)
	if safeName == "" || strings.Contains(safeName, "/") || strings.Contains(safeName, "\\") {
		return "", false
	}

	candidates := []string{
		filepath.FromSlash(filepath.Join("..", "website", "glowbom-oss", "public", safeName)),
		filepath.FromSlash(filepath.Join("website", "glowbom-oss", "public", safeName)),
		filepath.FromSlash(filepath.Join("..", "website", "glowby-oss", "public", safeName)),
		filepath.FromSlash(filepath.Join("website", "glowby-oss", "public", safeName)),
	}

	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		return abs, true
	}

	return "", false
}

func serveGlowbomPublicAsset(w http.ResponseWriter, r *http.Request, fileName string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	assetPath, ok := resolveGlowbomPublicAssetPath(fileName)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, assetPath)
}

func glowbomFaviconHandler(w http.ResponseWriter, r *http.Request) {
	serveGlowbomPublicAsset(w, r, "favicon.png")
}

func glowbomLogoSVGHandler(w http.ResponseWriter, r *http.Request) {
	serveGlowbomPublicAsset(w, r, "logo-svg.svg")
}

func glowbomBackendInfoPayload() backendInfo {
	info := backendInfo{
		Name:       "Glowbom OSS",
		Summary:    "Choose a project folder and let Glowbom OSS finish the engineering work locally.",
		HowItWorks: "Glowbom OSS manages agent drivers for you, keeps context in one place, and streams every step while code is being improved.",
		RuntimeURL: "http://127.0.0.1:" + getAgentPort(),
		Links: []backendInfoLink{
			{Label: "Glowbom OSS repository", URL: glowbomRepositoryURL},
			{Label: "Glowbom website", URL: glowbomWebsiteURL},
			{Label: "Codex App Server", URL: codexAppServerURL},
			{Label: "Backend metadata (JSON)", URL: "/opencode/about"},
			{Label: "Backend health", URL: "/opencode/health"},
			{Label: "Auth status", URL: "/opencode/auth/status"},
		},
		Drivers: []backendDriver{
			{
				ID:          "opencode",
				Label:       "OpenCode",
				Status:      "available",
				Description: "Default driver today. Great for local refine runs and broad model/provider flexibility.",
			},
			{
				ID:          "codex-app-server",
				Label:       "Codex App Server",
				Status:      "planned",
				Description: "Next planned driver. You will be able to switch drivers without changing your project workflow.",
			},
		},
	}

	if pdfPath, ok := resolveGlowbomDesktopPDFPath(); ok {
		info.ProjectDescriptionURL = "/opencode/about/project-description"
		info.ProjectDescriptionPath = pdfPath
		info.Links = append(info.Links, backendInfoLink{
			Label: "Glowbom Desktop system paper (PDF)",
			URL:   info.ProjectDescriptionURL,
		})
	}

	return info
}

func normalizedLinkHref(raw string) string {
	href := strings.TrimSpace(raw)
	if href == "" {
		return "#"
	}
	return href
}

func glowbomBackendHomeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	info := glowbomBackendInfoPayload()
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	var linksHTML strings.Builder
	for _, link := range info.Links {
		label := html.EscapeString(strings.TrimSpace(link.Label))
		href := html.EscapeString(normalizedLinkHref(link.URL))
		if label == "" || href == "" {
			continue
		}
		linksHTML.WriteString(fmt.Sprintf(`<li><a href="%s" target="_blank" rel="noreferrer">%s</a></li>`, href, label))
	}

	var driversHTML strings.Builder
	for _, driver := range info.Drivers {
		label := html.EscapeString(strings.TrimSpace(driver.Label))
		status := "Planned"
		if strings.EqualFold(strings.TrimSpace(driver.Status), "available") {
			status = "Available"
		}
		description := html.EscapeString(strings.TrimSpace(driver.Description))
		driversHTML.WriteString(fmt.Sprintf(
			`<li><strong>%s</strong> <span class="chip">%s</span><div class="driver-desc">%s</div></li>`,
			label,
			html.EscapeString(status),
			description,
		))
	}

	projectPathLine := `<p class="meta"><strong>Project description PDF:</strong> not found locally.</p>`
	if strings.TrimSpace(info.ProjectDescriptionPath) != "" {
		projectPathLine = fmt.Sprintf(
			`<p class="meta"><strong>Local PDF path:</strong> <code>%s</code></p>`,
			html.EscapeString(info.ProjectDescriptionPath),
		)
	}

	page := fmt.Sprintf(`<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <link rel="icon" type="image/png" href="/favicon.png" />
    <title>%s</title>
    <style>
      :root {
        color-scheme: light;
        --bg: #fafafa;
        --surface: #ffffff;
        --surface-soft: #f3f8f6;
        --text: #2b2b2b;
        --muted: #5c5c5c;
        --border: #dfe9e4;
        --brand-green: #29de92;
        --brand-cyan: #29ded3;
      }
      * { box-sizing: border-box; }
      body {
        margin: 0;
        min-height: 100vh;
        background: linear-gradient(180deg, #eaf0ef 0%%, #f1f4f3 32%%, #f8f8f8 100%%);
        color: var(--text);
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
      }
      .topbar {
        height: 80px;
        border-bottom: 1px solid #eef4f1;
        background: #ffffff;
      }
      .topbar-inner {
        max-width: 1420px;
        height: 100%%;
        margin: 0 auto;
        padding: 0 22px;
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 18px;
      }
      .topbar-logo {
        display: inline-flex;
        align-items: center;
      }
      .topbar-logo img {
        width: 118px;
        height: 29px;
        margin-top: 8px;
      }
      .topbar-links {
        display: flex;
        align-items: center;
        gap: 22px;
        margin-left: auto;
        margin-right: 8px;
      }
      .topbar-links a {
        color: var(--text);
        text-decoration: none;
        font-size: 16px;
        font-weight: 400;
      }
      .topbar-links a:hover {
        color: var(--brand-green);
        text-decoration: underline;
      }
      .button {
        border: none;
        border-radius: 999px;
        padding: 11px 18px;
        background-image: radial-gradient(circle farthest-side at 0%% 0%%, var(--brand-green), var(--brand-cyan));
        color: #ffffff;
        font: inherit;
        font-weight: 600;
        text-decoration: none;
      }
      main {
        width: min(1120px, 100%%);
        margin: 0 auto;
        padding: 24px 16px 34px;
        display: grid;
        gap: 12px;
      }
      .hero {
        border: 1px solid #e2f0ea;
        border-radius: 16px;
        background: linear-gradient(140deg, rgba(255, 255, 255, 0.98) 0%%, rgba(238, 250, 244, 0.98) 100%%);
        box-shadow: 0 18px 44px rgba(41, 222, 146, 0.14);
        padding: 22px 22px;
        display: grid;
        gap: 8px;
      }
      .card {
        border: 1px solid var(--border);
        border-radius: 16px;
        background: #ffffff;
        box-shadow: 0 8px 22px rgba(43, 43, 43, 0.08);
        padding: 16px;
        display: grid;
        gap: 8px;
      }
      h1 {
        margin: 0 0 6px;
        font-size: clamp(1.9rem, 1.15rem + 2.1vw, 2.7rem);
        line-height: 1;
      }
      h2 {
        margin: 0;
        font-size: 1.08rem;
      }
      p {
        margin: 0;
        line-height: 1.5;
      }
      ol, ul {
        margin: 0;
        padding-left: 20px;
        display: grid;
        gap: 7px;
      }
      a {
        color: #177e52;
        text-decoration: none;
        font-weight: 600;
      }
      a:hover { text-decoration: underline; }
      .meta {
        margin-top: 8px;
        color: #5c5c5c;
        font-size: 0.9rem;
      }
      code {
        background: #f1f6f3;
        border-radius: 6px;
        padding: 2px 6px;
      }
      .chip {
        display: inline-flex;
        align-items: center;
        border-radius: 999px;
        border: 1px solid #bfe6d3;
        background: #effbf5;
        color: #157047;
        font-size: 0.75rem;
        font-weight: 700;
        padding: 3px 9px;
        margin-left: 6px;
      }
      .driver-desc {
        color: var(--muted);
        font-size: 0.9rem;
        margin-top: 2px;
      }
      @media (max-width: 880px) {
        .topbar {
          height: 72px;
        }
        .topbar-inner {
          padding: 0 12px;
        }
        .topbar-links {
          display: none;
        }
        .button {
          padding: 9px 13px;
          font-size: 0.82rem;
        }
      }
    </style>
  </head>
  <body>
    <header class="topbar">
      <div class="topbar-inner">
        <a class="topbar-logo" href="https://glowbom.com" target="_blank" rel="noreferrer">
          <img alt="Glowbom" src="/logo-svg.svg" />
        </a>
        <nav class="topbar-links">
          <a href="https://glowbom.com/oss" target="_blank" rel="noreferrer">Glowbom OSS</a>
          <a href="https://glowbom.com/desktop/" target="_blank" rel="noreferrer">Desktop</a>
          <a href="https://glowbom.com/terms.html" target="_blank" rel="noreferrer">Terms</a>
          <a href="https://glowbom.com/pricing/" target="_blank" rel="noreferrer">Pricing</a>
          <a href="https://glowbom.com/docs/" target="_blank" rel="noreferrer">Docs</a>
          <a href="https://glowbom.com/#case_studies" target="_blank" rel="noreferrer">Apps</a>
        </nav>
        <a class="button" href="https://glowbom.com/draw" target="_blank" rel="noreferrer">Get Started for Free</a>
      </div>
    </header>

    <main>
      <section class="hero">
        <h1>%s</h1>
        <p>%s</p>
        <p class="meta">%s</p>
        <p class="meta">OpenCode runtime: <code>%s</code></p>
      </section>
      <section class="card">
        <h2>How It Works</h2>
        <ol>
          <li>Open the Glowbom OSS web UI at <code>http://127.0.0.1:4572</code>.</li>
          <li>Choose a local Glowbom project folder.</li>
          <li>Pick an agent driver and run <code>/opencode/refine</code>.</li>
          <li>Watch live logs, answer prompts, and open outputs in your IDE.</li>
        </ol>
      </section>
      <section class="card">
        <h2>Agent Drivers</h2>
        <ul>%s</ul>
      </section>
      <section class="card">
        <h2>Links</h2>
        <ul>%s</ul>
        %s
      </section>
    </main>
  </body>
</html>`,
		html.EscapeString(info.Name),
		html.EscapeString(info.Name),
		html.EscapeString(info.Summary),
		html.EscapeString(info.HowItWorks),
		html.EscapeString(info.RuntimeURL),
		driversHTML.String(),
		linksHTML.String(),
		projectPathLine,
	)

	_, _ = w.Write([]byte(page))
}

func openCodeAboutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(glowbomBackendInfoPayload())
}

func openCodeProjectDescriptionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pdfPath, ok := resolveGlowbomDesktopPDFPath()
	if !ok {
		http.Error(w, "Project description PDF not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, pdfPath)
}
