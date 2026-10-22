package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// designRules is get_design_system's guide to the warm editorial design system; the
// html_starter after it carries the same tokens as CSS.
const designRules = `shartifacts design system: warm editorial. A tinted cream canvas, regular-weight serif headlines with negative tracking over a humanist sans, one coral accent used scarcely, cream feature cards and dark product surfaces. It should read like a literary publication, not a SaaS template. Copy the style element from html_starter into HTML artifacts (a rendered style element for React) and use its variables and classes; never inline hex.

Colors (CSS variables)
- Brand: --primary #cc785c coral (primary CTAs, coral callouts, inline links), --primary-active #a9583e (pressed), --primary-disabled #e6dfd8, --accent-teal #5db8a6 (status dots, live indicators, sparingly), --accent-amber #e8a55a (category badges, inline highlights).
- Surfaces: --canvas #faf9f5 (page floor; never pure white or cool gray), --surface-soft #f5f0e8 (soft bands, dividers), --surface-card #efe9de (feature cards), --surface-cream-strong #e8e0d2 (selected tabs, emphasized bands), --surface-dark #181715 (code, product mockups, featured cards, footer), --surface-dark-elevated #252320 (cards and buttons inside dark bands), --surface-dark-soft #1f1e1b (code blocks inside dark cards), --hairline #e6dfd8 (1px borders), --hairline-soft #ebe6df (faint dividers).
- Text: --ink #141413 (headlines, primary text), --body-strong #252523 (lead text), --body #3d3d3a (running text), --muted #6c6a64 (sub-heads, breadcrumbs), --muted-soft #8e8b82 (captions, fine print), --on-primary #ffffff, --on-dark #faf9f5, --on-dark-soft #a09d96.
- Semantic: --success #5db872, --warning #d4a017, --error #c64545.

Typography
- Display (h1-h3): --font-display, Copernicus/Tiempos Headline with Cormorant Garamond as the open substitute. Always weight 400 with negative tracking; never bold. When in doubt, go bigger, not bolder.
- Body, navigation, buttons, labels: --font-body, StyreneB with Inter as the substitute, 400 for text and 500 for labels. Code and terminal text: --font-code, JetBrains Mono.
- The fonts load from this server (/vendor/fonts) via the starter's @font-face rules, with fallback stacks elsewhere.
- Scale as size/line-height/tracking:
  - display-xl 64/1.05/-1.5px (hero h1), display-lg 48/1.1/-1px (section heads), display-md 36/1.15/-0.5px, display-sm 28/1.2/-0.3px (callout headlines, prices).
  - title-lg 22/1.3 500, title-md 18/1.4 500 (card titles), title-sm 16/1.4 500 (tile titles, list labels).
  - body-md 16/1.55, body-sm 14/1.55.
  - caption 13/1.4 500 (badges), caption-uppercase 12/1.4 500 with 1.5px tracking (.eyebrow, category tags).
  - code 14/1.6, button and nav-link 14 500.

Layout
- 4px base; spacing tokens --space-xxs 4, xs 8, sm 12, md 16, lg 24, xl 32, xxl 48, section 96.
- 96px between major bands. Cards take 32px padding (24px for code windows and tiles), coral callouts 48px, and large CTA bands 64px.
- Content is at most 1200px wide and centered (.artifact-page). Heroes split 6/6, with the headline, sub-line and buttons left and the illustration or code card right.
- Grids collapse rather than shrink: feature cards go 3 to 2 to 1, tiles 4-6 to 3 to 2.
- Breakpoints: mobile under 768px (stack everything, h1 down to 32px), tablet 768-1024, desktop above.
- Compact tools may tighten the spacing, but keep the type scale.

Shape and depth
- Radii: --radius-xs 4 (tiny accents), sm 6 (small inline buttons), md 8 (buttons, inputs, tabs), lg 12 (cards), xl 16 (hero containers), pill 9999 (badges, avatars, circular icon buttons).
- Depth is color-block first: cream against cream-card against dark. Shadows are rare, and at most 0 1px 3px rgba(20,20,19,0.08).
- Avatars are 40px circles. Illustrations are simple line art in coral and dark strokes on cream, never photoreal. Prefer showing real code or product UI on dark cards.

Components (classes in the starter)
- button.primary: coral, white 14px/500 text, 40px tall, 12px 20px padding, 8px radius; darkens to --primary-active when pressed.
- Plain button (secondary): canvas with a hairline border, same size.
- On dark surfaces (.card-dark button) buttons stay dark elevated with --on-dark text; never invert them to light.
- Circular icon buttons: 36px pill, canvas with a hairline border.
- Text links are coral.
- Inputs: canvas, 40px, 10px 14px padding, hairline border, 8px radius. On focus the border turns coral with a 3px coral ring at 15% alpha (--focus-ring).
- .card: feature card in --surface-card, 12px radius, 32px padding, holding an icon, a title-md heading and body text.
- .card-outline: canvas with a hairline border, for comparison, pricing and connector tiles.
- .card-dark: product mockup or featured tier, --surface-dark with --on-dark text.
- pre: the code window, dark with JetBrains Mono, scrolling horizontally rather than wrapping. Muted syntax colors (warm oranges, soft blues, grays), with line numbers in --muted-soft.
- .callout: full-bleed coral card, white text, 48px padding, with a canvas button inside.
- Badges: .badge is a pill in --surface-card with 13px/500 ink text, 4px 12px padding. .badge-coral is coral with 12px uppercase text tracked 1.5px, for NEW and BETA.
- .category-tab: muted text on transparent, 8px 14px padding, 8px radius. Active is --surface-card with ink text.
- Top nav: 64px, canvas, 14px/500 links, with a coral primary CTA at the right.
- Toasts and consent cards: dark, 12px radius, 24px padding.
- Footer: dark (--surface-dark), --on-dark-soft text, 64px vertical padding.
- A small radial asterisk glyph (✳) may prefix a wordmark or mark a list. Never use Claude or Anthropic names or logos.

Rules
- Use coral scarcely on individual elements and generously only on full-bleed callouts.
- Alternate surface modes between bands: cream, cream card, dark, cream, coral callout, dark footer.
- There are three surface tones (cream, coral, dark navy). Add no fourth: no purple, blue or green sections, no gradients.
- No hover treatments. Only the primary button changes, darkening on press.
- Never use a sans for display headlines, and never use cool gray or white canvases.

Explicit user instructions take priority. Preserve existing designs when editing. Source is never styled automatically.`

type uploadInfo struct {
	BaseURL      string `json:"base_url"`
	UploadURL    string `json:"upload_url"`
	Method       string `json:"method"`
	ContentType  string `json:"content_type"`
	MaxFileBytes int64  `json:"max_file_bytes"`
	CurlExample  string `json:"curl_example"`
	Instructions string `json:"instructions"`
}

func rawJSON(v map[string]any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) mcpServer(baseURL, uploadPath string) *mcp.Server {
	claudeNote := ""
	if s.Claude != nil {
		claudeNote = "\nArtifacts can call Claude with await window.claude.complete(prompt) (or an array of {role, content} messages); it resolves to the reply text.\n"
	}
	links := s.linkBase(baseURL)
	info := uploadInfo{
		BaseURL: baseURL, UploadURL: baseURL + uploadPath, Method: "POST",
		ContentType:  "The file's MIME type, e.g. video/mp4",
		MaxFileBytes: store.MaxMedia,
		CurlExample:  "curl --fail-with-body --silent --show-error -H 'Content-Type: video/mp4' --data-binary '@./clip.mp4' --url '" + strings.ReplaceAll(baseURL+uploadPath, "'", "'\"'\"'") + "'",
		Instructions: "Run curl in the agent's shell with the local file and its MIME type; send raw bytes, not JSON or multipart. Embed the returned url literally. Same host/port as this MCP request; the shell must reach it. Never transcribe base64.",
	}
	uploadNote := ""
	if uploadPath != "/api/media" {
		// With sign-in on, the shell has no token: the URL carries an hour's grant instead.
		uploadNote = "\nThe upload URL works for an hour without a token; list_media returns a fresh one."
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "shartifacts", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: `Create and iterate on artifacts on this local network. Kinds: html, react, markdown, slides (Markdown slides separated by --- lines; "Note:" starts speaker notes), svg, mermaid, code (name the title with a file extension for highlighting). Markdown and slides render $TeX$ math and fenced mermaid. Larger artifacts can add text files (CSS, JS modules, components, JSON, more pages) with files; reference them by relative path, and React imports them relatively (./Chart). React must default-export a component and may import react, react-dom, recharts, lucide-react, d3, lodash, papaparse, mathjs, three, chart.js, xlsx, tone, plotly and shadcn/ui (@/components/ui/*); Tailwind classes work. HTML may load those libraries with their usual cdnjs/jsdelivr/unpkg script tags or cdn.tailwindcss.com (served locally); no other external URLs, iframes or fetches. Artifacts get Claude's capability runtime: await claude.use(name) for user, db (realtime documents; agents use artifact_db), assets, artifact (publish itself), comments, room (presence and messages), downloads, permissions, files (the Media library), mcp (this server's connectors and artifacts_data) and sample (Claude, when enabled); no manifest is needed, and capabilities.db.rules declare db access. The older window.storage API (artifact_storage), window.fs.readFile and window.claude.complete work too. Forms, alert/confirm, downloads and links (new tab) work. search_artifacts finds artifacts by their words (typos are fine) and filters; give new artifacts a description and a few tags so people can find them. Users leave comment threads, some marked to_claude (open_comments in list_artifacts; search_artifacts has:open finds them); answer and resolve them with artifact_comments. watch_artifacts waits for new versions, comments and data. Delete only when asked, with delete_artifact's confirmation.

Media (video, audio, images, WebVTT; MP4 H.264/AAC or WebM preferred) and data files (CSV, JSON, text, Markdown, XLSX, PDF): upload files. Add ?name=FILE to the upload URL to keep a file's name; artifacts read data files with await window.fs.readFile(name, {encoding: 'utf8'}) (omit encoding for bytes). Do not manually transcribe base64 into source (2 MiB limit). Shell agents: run the curl command from list_media (raw bytes, max 256 MiB). Only clients that can supply bytes programmatically use begin_media_upload then upload_media_chunk (<=1048576 bytes per chunk, use returned offset, final:true on last). If the shell cannot reach the upload host, say so. list_media returns reusable URLs; embed them literally, e.g. <video controls playsinline src="/media/ID.mp4">, <audio controls src="/media/ID.mp3">, <img src="/media/ID.png">, <track kind="captions" src="/media/ID.vtt" srclang="en"> (set crossorigin="anonymous" on the parent). Autoplay needs muted playsinline. Verify media decodes/plays in the saved preview; report missing or corrupt media instead of claiming success.

Apps are full-stack micro frontends shown as tabs on the home page: a React/TypeScript frontend and an optional Go (or prebuilt Rust) backend with its own data, deployed as numbered versions you can preview, promote and roll back, and shared through public links. Three tools: get_app (guide:true first: how to build one here), deploy_app (create, then ship versions) and manage_app (promote, update, restart, call, share, delete).

Before updating, get_artifact for the latest base_version. For small changes send edits (old_str/new_str copied exactly from the latest source) instead of resending content. Give people links on ` + links + `, e.g. ` + links + `/a/{id}, even if you connected through another address; the upload URL below is for your shell, not for people.

New visual artifacts use the get_design_system default. Explicit user design instructions always override it. Preserve an existing artifact's design when editing; never restyle supplied source or plain code/data/diagrams.
` + claudeNote + "\nUpload URL: " + info.UploadURL + "\nShell example (replace ./clip.mp4 with the file on the agent machine): " + info.CurlExample + uploadNote})
	type ListInput struct {
		Limit  int    `json:"limit,omitempty" jsonschema:"At most this many (default 50, max 200)"`
		Cursor string `json:"cursor,omitempty" jsonschema:"The next_cursor of the previous page, for the page after it"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "list_artifacts", Description: "List artifacts, most recently updated first, 50 at a time (id, title, kind, version, description, icon, tags, pinned, open_comments); total counts them all and next_cursor fetches the next page. To find something specific, search_artifacts is quicker."}, func(ctx context.Context, req *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, any, error) {
		all := s.Store.List()
		page, next, err := pageOf(all, func(a store.Summary) (time.Time, string) { return a.UpdatedAt, a.ID }, in.Cursor, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		out := map[string]any{"artifacts": page, "total": len(all)}
		if next != "" {
			out["next_cursor"] = next
		}
		return nil, out, nil
	})
	type SearchInput struct {
		Query string `json:"query" jsonschema:"Words to find in titles, tags, descriptions, rendered text, notes and comments (typos and word forms are fine), with optional filters: \"exact phrase\", -exclude, kind:react, tag:finance or #finance, is:pinned, has:comments|open|files|video|audio|image|chart|diagram|table|form|3d|math|data, updated:today|week|month|year|30d|2026-03, created:…, before:/after: dates"`
		Sort  string `json:"sort,omitempty" jsonschema:"relevance (default with words), updated, created or title"`
		Limit int    `json:"limit,omitempty" jsonschema:"At most this many results (default 10, max 50)"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "search_artifacts", Description: "Find artifacts by what they contain or are about, ranked, with a matching excerpt for each and a corrected query when words were misspelt. Use it before list_artifacts when looking for something specific."}, func(ctx context.Context, req *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		res := s.runSearch(in.Query, nil, in.Sort, min(limit, 50))
		type result struct {
			ID          string   `json:"id"`
			Title       string   `json:"title"`
			Kind        string   `json:"kind"`
			Version     int      `json:"version"`
			UpdatedAt   string   `json:"updated_at"`
			Description string   `json:"description,omitempty"`
			Tags        []string `json:"tags,omitempty"`
			Matched     []string `json:"matched,omitempty"`
			Excerpt     string   `json:"excerpt,omitempty"`
		}
		out := []result{}
		for _, r := range res.Results {
			var excerpt strings.Builder
			for _, seg := range r.Snippet {
				excerpt.WriteString(seg.Text)
			}
			out = append(out, result{r.Summary.ID, r.Title, r.Kind, r.Version, r.UpdatedAt.Format("2006-01-02"), r.Description, r.Summary.Tags, r.Matched, excerpt.String()})
		}
		body := map[string]any{"results": out, "total": res.Total}
		if res.Suggestion != "" {
			body["did_you_mean"] = res.Suggestion
		}
		return nil, body, nil
	})
	type GetInput struct {
		ID      string `json:"id" jsonschema:"Artifact ID"`
		Version int    `json:"version,omitempty" jsonschema:"One revision instead of the latest"`
		History bool   `json:"history,omitempty" jsonschema:"Return every revision, not just the latest"`
	}
	// saved acknowledges a write without echoing source the caller just sent.
	saved := func(a store.Artifact) map[string]any {
		r := a.Revisions[len(a.Revisions)-1]
		out := map[string]any{"id": a.ID, "version": r.Version, "title": r.Title, "kind": r.Kind}
		if problems := checkArtifact(r.Kind, r.Content, r.Files); len(problems) > 0 {
			out["warnings"] = problems
		}
		return out
	}
	mcp.AddTool(m, &mcp.Tool{Name: "get_artifact", Description: "Read an artifact. Returns the latest revision; version for one revision; history:true for all. runtime_errors lists errors browsers hit in that version."}, func(ctx context.Context, req *mcp.CallToolRequest, in GetInput) (*mcp.CallToolResult, any, error) {
		a, e := s.Store.Get(in.ID)
		if e != nil {
			return nil, nil, e
		}
		version := len(a.Revisions)
		switch {
		case in.Version != 0:
			if in.Version < 1 || in.Version > len(a.Revisions) {
				return nil, nil, fmt.Errorf("version %d does not exist; latest is %d", in.Version, len(a.Revisions))
			}
			version = in.Version
			a.Revisions = a.Revisions[in.Version-1 : in.Version]
		case !in.History:
			a.Revisions = a.Revisions[len(a.Revisions)-1:]
		}
		return nil, struct {
			store.Artifact
			RuntimeErrors []RuntimeError `json:"runtime_errors,omitempty"`
		}{a, s.errors.forVersion(a.ID, version)}, nil
	})
	type CreateInput struct {
		Title       string   `json:"title"`
		Kind        string   `json:"kind"`
		Content     string   `json:"content"`
		Description string   `json:"description,omitempty" jsonschema:"One sentence for the library card"`
		Icon        string   `json:"icon,omitempty" jsonschema:"One short generic word for its icon (chart, calendar, recipe, code, map, game, list, money, timer, music, photo, book) or an emoji"`
		Tags        []string `json:"tags,omitempty" jsonschema:"A few short lowercase labels people would search or group it by (finance, recipes, work)"`
		// Capabilities are declared as on Claude's Artifact tool; db.rules take effect.
		Capabilities map[string]any    `json:"capabilities,omitempty" jsonschema:"Optional, as Claude's: {db: {rules: [{path, read, write}]}} sets who reads and writes which documents"`
		Files        map[string]string `json:"files,omitempty" jsonschema:"Optional supporting text files by relative path (style.css, components/Chart.tsx, data.json, about.html); the source references them relatively"`
		Note         string            `json:"note,omitempty"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "create_artifact", Description: "Create an artifact (title, kind, content, optional description, tags, files and note). Returns id, version, title, kind, and warnings for problems the preview will hit."}, func(ctx context.Context, req *mcp.CallToolRequest, in CreateInput) (*mcp.CallToolResult, any, error) {
		files := map[string]*string{}
		for p, body := range in.Files {
			files[p] = &body
		}
		a, e := s.Store.Save("", store.Input{Title: in.Title, Kind: in.Kind, Content: in.Content, Files: files, Note: in.Note, Description: &in.Description, Icon: &in.Icon, Tags: &in.Tags, Capabilities: rawJSON(in.Capabilities)})
		if e != nil {
			return nil, nil, e
		}
		return nil, saved(a), nil
	})
	type UpdateInput struct {
		ID           string             `json:"id"`
		Title        string             `json:"title,omitempty" jsonschema:"Omit to keep"`
		Kind         string             `json:"kind,omitempty" jsonschema:"Omit to keep"`
		Content      *string            `json:"content,omitempty" jsonschema:"Full new source; omit to keep or when sending edits"`
		Edits        []store.Edit       `json:"edits,omitempty" jsonschema:"old_str/new_str pairs against the latest source; each old_str copied exactly and occurring once; applied in order, all or none"`
		Files        map[string]*string `json:"files,omitempty" jsonschema:"Supporting files to add or replace by path; null removes one; files not named are kept"`
		Description  *string            `json:"description,omitempty" jsonschema:"Omit to keep"`
		Icon         *string            `json:"icon,omitempty" jsonschema:"Omit to keep"`
		Tags         *[]string          `json:"tags,omitempty" jsonschema:"Replaces the tags; omit to keep"`
		Capabilities map[string]any     `json:"capabilities,omitempty" jsonschema:"Replaces the declared capabilities; omit to keep"`
		Note         string             `json:"note,omitempty"`
		BaseVersion  int                `json:"base_version" jsonschema:"Latest version from get_artifact"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "update_artifact", Description: "Append a revision. Prefer edits for small changes, content for rewrites. Pass the latest base_version; on conflict re-read. Returns id, version, title, kind, and any warnings."}, func(ctx context.Context, req *mcp.CallToolRequest, in UpdateInput) (*mcp.CallToolResult, any, error) {
		next := store.Input{Title: in.Title, Kind: in.Kind, Edits: in.Edits, Files: in.Files, Note: in.Note, Description: in.Description, Icon: in.Icon, Tags: in.Tags, Capabilities: rawJSON(in.Capabilities), BaseVersion: in.BaseVersion}
		switch {
		case in.Content != nil && in.Edits != nil:
			return nil, nil, fmt.Errorf("send content or edits, not both")
		case in.Content != nil:
			next.Content = *in.Content
		case in.Edits == nil:
			next.Edits = []store.Edit{} // neither: keep the latest source
		}
		a, e := s.Store.Save(in.ID, next)
		if e != nil {
			return nil, nil, e
		}
		return nil, saved(a), nil
	})
	type StorageInput struct {
		ID string `json:"id" jsonschema:"Artifact ID"`
		storageOp
	}
	mcp.AddTool(m, &mcp.Tool{Name: "artifact_storage", Description: "Read or write the data an artifact keeps with window.storage (op get|set|delete|list; shared for the shared scope)."}, func(ctx context.Context, req *mcp.CallToolRequest, in StorageInput) (*mcp.CallToolResult, any, error) {
		result, e := s.runStorage(in.ID, in.storageOp)
		if e == nil && result == nil {
			result = map[string]any{"key": in.Key, "found": false, "shared": in.Shared}
		}
		return nil, result, e
	})
	s.addCommentTool(m)
	s.addAgentTools(m)
	s.addAppTools(m, baseURL, links)
	type ListMediaInput struct {
		Name   string `json:"name,omitempty" jsonschema:"Only files whose name contains this, in any case"`
		Type   string `json:"type,omitempty" jsonschema:"Only this MIME type or family, e.g. video or image/png"`
		Limit  int    `json:"limit,omitempty" jsonschema:"At most this many (default 50, max 200)"`
		Cursor string `json:"cursor,omitempty" jsonschema:"The next_cursor of the previous page, for the page after it"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "list_media", Description: "List uploaded media, newest first, 50 at a time (url, MIME type, size, name; URLs are reusable), filtered by name or type; total counts the matches and next_cursor fetches the next page. Also returns the upload URL and a ready-to-run curl command for local files."}, func(ctx context.Context, req *mcp.CallToolRequest, in ListMediaInput) (*mcp.CallToolResult, any, error) {
		items, err := s.Store.ListMedia()
		if err != nil {
			return nil, nil, err
		}
		name, kind := strings.ToLower(strings.TrimSpace(in.Name)), strings.ToLower(strings.TrimSpace(in.Type))
		matches := items[:0]
		for _, it := range items {
			if (name == "" || strings.Contains(strings.ToLower(it.Name), name)) && (kind == "" || it.MIMEType == kind || strings.HasPrefix(it.MIMEType, strings.TrimSuffix(kind, "/")+"/")) {
				matches = append(matches, it)
			}
		}
		sort.SliceStable(matches, func(i, j int) bool { return matches[i].UploadedAt.After(*matches[j].UploadedAt) })
		page, next, err := pageOf(matches, func(m store.Media) (time.Time, string) { return *m.UploadedAt, m.ID }, in.Cursor, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		out := map[string]any{"media": page, "total": len(matches), "upload_url": info.UploadURL, "base_url": info.BaseURL, "method": info.Method, "max_file_bytes": info.MaxFileBytes, "curl_example": info.CurlExample}
		if next != "" {
			out["next_cursor"] = next
		}
		return nil, out, nil
	})
	if !s.NoChunkUpload {
		type BeginMediaInput struct {
			MIMEType string `json:"mime_type" jsonschema:"File MIME type, e.g. video/mp4"`
		}
		mcp.AddTool(m, &mcp.Tool{Name: "begin_media_upload", Description: "Begin a chunked base64 upload (programmatic clients only; shells use list_media). Upload URL: " + info.UploadURL}, func(ctx context.Context, req *mcp.CallToolRequest, in BeginMediaInput) (*mcp.CallToolResult, any, error) {
			u, e := s.Store.BeginMediaUpload(in.MIMEType)
			return nil, struct {
				store.MediaUpload
				UploadURL   string `json:"upload_url"`
				CurlExample string `json:"curl_example"`
			}{u, info.UploadURL, info.CurlExample}, e
		})
		type ChunkInput struct {
			UploadID string `json:"upload_id"`
			Offset   int64  `json:"offset" jsonschema:"Byte offset from the previous call; starts at 0"`
			Data     string `json:"data_base64" jsonschema:"Base64 of <=1048576 bytes"`
			Final    bool   `json:"final" jsonschema:"True on the last chunk"`
		}
		mcp.AddTool(m, &mcp.Tool{Name: "upload_media_chunk", Description: "Append a chunk at the returned offset. final:true commits and returns {id,url,mime_type,size}; otherwise the next offset."}, func(ctx context.Context, req *mcp.CallToolRequest, in ChunkInput) (*mcp.CallToolResult, any, error) {
			if len(in.Data) > base64.StdEncoding.EncodedLen(store.MediaChunkSize) {
				return nil, nil, fmt.Errorf("chunk exceeds 1 MiB; split it before base64 encoding")
			}
			b, e := base64.StdEncoding.DecodeString(in.Data)
			if e != nil {
				return nil, nil, fmt.Errorf("invalid base64: %w", e)
			}
			result, e := s.Store.AppendMedia(in.UploadID, in.Offset, b, in.Final)
			return nil, result, e
		})
	}

	mcp.AddTool(m, &mcp.Tool{Name: "get_design_system", Description: "Get the default artifact design: rules plus an HTML starter that embeds the shared CSS."}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		css, e := fs.ReadFile(s.Web, "design-system.css")
		if e != nil {
			return nil, nil, fmt.Errorf("design system unavailable: %w", e)
		}
		starter := "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>Your artifact</title><style>" + string(css) + "</style></head><body><main class=\"artifact-page\"><section><p class=\"eyebrow\">A small idea</p><h1>Your next idea.</h1><p>A little space to make something useful.</p><button class=\"primary\">Try it</button> <button>Learn more</button></section><section class=\"card\"><h3>Start here</h3><p>Replace this with your content.</p></section></main></body></html>"
		// Plain text, not JSON: avoids escaping the stylesheet and sending it twice (content + structuredContent).
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: designRules + "\n\nhtml_starter:\n" + starter}}}, nil, nil
	})
	return m
}
