<div align="center">

# Zilan · 知澜

**Where knowledge converges into waves — bring every document to life**

Minimalist aesthetics × Deep parsing × Autonomous reasoning — the next-generation enterprise knowledge hub

</div>

<p align="center">
    <a href="./LICENSE">
        <img src="https://img.shields.io/badge/License-MIT-ffffff?labelColor=d4eaf7&color=2e6cc4" alt="License">
    </a>
    <a href="./CHANGELOG.md">
        <img alt="Version" src="https://img.shields.io/badge/version-1.0.0-2e6cc4?labelColor=d4eaf7">
    </a>
    <a>
        <img alt="Deployment" src="https://img.shields.io/badge/Deployment-Private·Offline-333333?labelColor=f2f2f2">
    </a>
    <a>
        <img alt="Design Language" src="https://img.shields.io/badge/Design-Monochrome Minimal-333333?labelColor=f2f2f2">
    </a>
</p>

<p align="center">
| <b>English</b> | <a href="./README_CN.md"><b>简体中文</b></a> |
</p>

<p align="center">
  <h4 align="center">

  [Overview](#-overview) • [Highlights](#-highlights) • [Architecture](#-architecture) • [Feature Overview](#-feature-overview) • [Getting Started](#-getting-started) • [Docs](#-docs) • [Developer Guide](#-developer-guide)

  </h4>
</p>

# 💡 Zilan — Where Knowledge Converges: a Minimalist Enterprise Knowledge Hub Unifying RAG, Agent Reasoning, and Auto-Wiki

## 📌 Overview

**Zilan (知澜)** is an LLM-powered enterprise knowledge management and intelligent Q&A platform, engineered toward an *ima-class* minimalist product experience that unifies **RAG quick Q&A**, **ReAct agent reasoning**, and **auto-generated Wiki knowledge distillation**.

Zilan is a thorough re-imagination built on a battle-tested open-source knowledge framework, carrying product genes entirely its own:

- **🎨 A brand-new minimalist design language** — no clutter, no visual noise: a dark monochrome brand color (#333 family), large rounded corners, feather-light shadows, and pure solid backgrounds; one consistent visual system from the login page through chat streams to the settings center — quiet, restrained, and content-first
- **📄 A deep document parsing pipeline** — structured PDF table extraction, mathematical formula recognition, and dual-column layout awareness; **automatic routing** selects parsing engine intensity per document profile, low-scoring results are retried with heavier engines, and stubborn documents land in a human review queue
- **🕸️ GraphRAG-enhanced retrieval** — entity-relation storage on Neo4j; **community summaries + local subgraph retrieval** complement dense/sparse vector recall, widening coverage and explainability for complex multi-hop questions
- **🧠 Memory & knowledge distillation system (work in progress)** — an ima-aligned three-layer memory architecture: L1 working memory (current conversation), L2 short-term memory (vectorized summaries of recent conversations), L3 long-term memory (user profile + fact triples + to-dos); memories are extracted asynchronously after each session and injected into the system prompt scored by `semantic similarity × time decay × (1 + log(access count))`; users can view, edit, and delete everything the AI remembers — GDPR-compliant by design
- **🔐 An independent identity and protocol stack** — a fully self-owned namespace across branding, JWT audience (`aud=zilan`), webhook signature (`X-Zilan-Signature`), and embed SDK (`zilan-widget.js`) — zero upstream traces

On top of that, Zilan inherits enterprise-grade engineering capabilities proven at scale: multi-source ingestion (Feishu / Notion / Yuque / RSS, and growing), 20+ LLM provider integrations, a dozen IM channels, website embed widgets, a 4-tier RBAC role matrix with workspace audit logs, AES-256-GCM credential encryption at rest, full-stack Langfuse observability plus a runtime task-queue governance dashboard, and a fully self-hostable modular architecture — LLMs, vector databases, and storage backends are all swappable, keeping data sovereignty entirely yours.

## ✨ Highlights

### 🎨 Minimalist Design Language · An Interface Built for Focus

| Principle | Details |
|-----------|---------|
| Dark monochrome brand | Brand color stays in the #333 family; interaction states shift luminance only — no flashy gradients, no saturated clashes |
| Large radii × light shadows | A global radius system (starting at 8/10/12px); cards and chat bubbles use feather-light or no shadows |
| Pure solid backgrounds | Pages return to solid colors (light #fafafa / dark #181818) — no textures, no decorative animations |
| Centered login | A single vertical focal point of brand mark + form; every login-irrelevant element removed |
| Symmetric chat bubbles | User-side dark, assistant-side light-gray symmetric rounded bubbles with inline citations and reasoning chains |
| Responsive | Consistent desktop / tablet / mobile experience; dark mode follows the system |

### 📄 Deep Document Parsing · Read Every PDF Properly

| Capability | Details |
|------------|---------|
| Table extraction | Detects table boundaries and restores them as Markdown tables, preserving row/column semantics |
| Formula recognition | Extracts mathematical formulas and converts them to LaTeX for fine-grained retrieval in academic and technical docs |
| Dual-column awareness | Detects two-/multi-column layouts and restores the correct reading order |
| Auto routing | Picks light or heavy parsing engines by file size and content profile, balancing throughput and quality |
| Quality scoring & retry | Parse results are auto-scored; short or low-scoring content is retried with a heavier engine |
| Human review queue | Documents that exhaust retries enter a review queue for human-in-the-loop closure |

### 🕸️ GraphRAG-Enhanced Retrieval

Entities and relations are extracted into Neo4j at ingest time to build a knowledge graph; at query time, **community summaries** provide the global view while **local subgraph retrieval** supplies neighborhood detail — fused with BM25 / dense vector recall via RRF, markedly improving answers to multi-hop and cross-document aggregation questions.

### 🧠 Memory & Knowledge Distillation (Work in Progress)

Toward an ima-class personal knowledge assistant, Zilan is building a three-layer memory architecture:

- **L1 working memory** — full context and intermediate state of the current conversation
- **L2 short-term memory** — vectorized summaries of the last N conversations (pgvector), recalled by relevance at session start
- **L3 long-term memory** — user profile, fact triples (subject-relation-object), and to-do items: structured tables + vector indexes + an optional image library

Supporting mechanics: memory is **extracted asynchronously** after each session via the Asynq task queue (fact triples / to-dos / emotional feedback); injection scores each memory as `score = semantic similarity × time decay × (1 + log(access count))` and loads the Top-K into the system prompt; high-value conversations (favorited or heavily followed-up) auto-distill into Wiki pages linked back to the source session; and users can **view, edit, and delete** everything the AI remembers — GDPR / privacy compliant.

### 🔐 Independent Brand · Independent Protocol

Product name, UI marks, browser titles, storage keys, JWT audience, webhook signature header, and embed SDK are unified under the Zilan namespace — a fully self-owned product identity.

## 📱 Interface Showcase

<table>
  <tr>
    <td colspan="2" align="center"><b>💬 Intelligent Q&A Conversation</b><br/><img src="./docs/images/qa.png" alt="Intelligent Q&A Conversation" width="100%"></td>
  </tr>
  <tr>
    <td width="50%" align="center"><b>📖 Wiki Browser</b><br/><img src="./docs/images/wiki-browser.png" alt="Wiki Browser" width="100%"></td>
    <td width="50%" align="center"><b>🕸️ Wiki Knowledge Graph</b><br/><img src="./docs/images/wiki-graph.png" alt="Wiki Knowledge Graph" width="100%"></td>
  </tr>
  <tr>
    <td width="50%" align="center"><b>🤖 Agent Mode · Tool Call Process</b><br/><img src="./docs/images/agent-qa.png" alt="Agent Mode Tool Call Process" width="100%"></td>
    <td width="50%" align="center"><b>⚙️ Conversation Settings</b><br/><img src="./docs/images/settings.png" alt="Conversation Settings" width="100%"></td>
  </tr>
  <tr>
    <td colspan="2" align="center"><b>🔭 Observability · Langfuse Tracing</b><br/><img src="./docs/images/langfuse.png" alt="Observability Langfuse Tracing" width="100%"></td>
  </tr>
</table>

## 🏗️ Architecture

![zilan-architecture](./docs/images/architecture.png)

Fully modular pipeline from document parsing, vectorization, and retrieval to LLM inference — every component is swappable and extensible. Supports local / private cloud deployment with full data sovereignty and a zero-barrier Web UI for quick onboarding.

## 🧩 Feature Overview

**Intelligent Conversation**

| Capability | Details |
|------------|---------|
| Intelligent Reasoning | ReACT progressive multi-step reasoning, autonomously orchestrating knowledge retrieval, MCP tools, and web search |
| Quick Q&A | RAG-based Q&A over knowledge bases for fast and accurate answers |
| Wiki Mode | Agent-driven auto-generation of structured, interlinked markdown Wiki pages from raw documents |
| Tool Calling | Built-in tools, MCP tools (incl. OAuth2 remote services, mid-conversation OAuth), web search; `@Skill / @MCP` mentions to scope the agent runtime per turn |
| Conversation Strategy | Online Prompt editing, retrieval threshold tuning, multi-turn context awareness, per-agent citation output toggle |
| Suggested Questions | Auto-generated question suggestions and after-answer follow-ups based on knowledge base content |
| Temporary Attachments | Session-scoped image / document uploads with async parsing for one-off Q&A, with a combined image + attachment limit |
| Citations & RAG Progress | Inline citation popovers and a references drawer (web / KB source distinction), shared markdown rendering, and stage-by-stage RAG pipeline progress in chat |
| Session Management | Filter and group sidebar sessions by source (Web / IM / Embed), with inline session-title rename |
| Memory & Personalization | Three-layer memory architecture in progress (see [Highlights](#-memory--knowledge-distillation-work-in-progress)) targeting cross-session personalization and proactive knowledge distillation |

**Knowledge Management**

| Capability | Details |
|------------|---------|
| Knowledge Base Types | FAQ / Document / Wiki with folder import, URL import, multi-tag management, and online entry |
| Deep Parsing Pipeline | PDF table extraction / formula recognition / dual-column awareness / auto routing / quality-scored retry / human review queue |
| Per-Upload Process Config | Override parser, chunking, multimodal (VLM / ASR), graph extraction, and question generation per upload batch via upload-confirm dialog or `process_config` API; reparse with new settings |
| Batch Reparse | Re-queue parsing for multiple documents at once with optional per-batch `process_config` |
| Data Source Import | Auto-sync from Feishu / Notion / Yuque / RSS feeds (more data sources coming soon); incremental and full sync |
| Document Formats | PDF / Word / Txt / Markdown / HTML / EPUB / MHTML / Images / CSV / Excel / PPT / JSON |
| Retrieval Strategies | BM25 sparse / Dense retrieval / GraphRAG (community summary + local subgraph) / parent-child chunking / HNSW-accelerated pgvector (1024-dim) / multi-dimensional indexing |
| Batch Selection | Marquee drag-select multiple documents in the KB list for batch operations |
| E2E Testing | Full-pipeline visualization with recall hit rate, BLEU / ROUGE metric evaluation |

**Integrations & Extensions**

| Capability | Details |
|------------|---------|
| LLMs | OpenAI / Azure OpenAI / Anthropic (Claude) / DeepSeek / Qwen (Alibaba Cloud) / Zhipu / Hunyuan / Doubao (Volcengine) / Gemini / MiniMax / NVIDIA / Novita AI / SiliconFlow / OpenRouter / Requesty / Ollama |
| Embeddings | Ollama / BGE / GTE / Zhipu / OpenAI-compatible APIs |
| Vector DBs | PostgreSQL (pgvector) / Elasticsearch / OpenSearch / Milvus / Weaviate / Qdrant / Apache Doris / Tencent VectorDB |
| Object Storage | Local / Tencent COS / Volcengine TOS / MinIO / AWS S3 / Alibaba Cloud OSS / Kingsoft Cloud KS3 / Huawei Cloud OBS; **multiple storage instances per workspace** with per-KB binding and a default instance |
| IM Channels | WeCom / Feishu / Lark (Feishu International) / QQBot / Slack / Telegram / DingTalk / Mattermost / WeChat / Yunzhijia |
| Website Embed | Publish agents to any site via `zilan-widget.js` with domain allowlists, rate limits, and secure-mode token exchange |
| Web Search | DuckDuckGo / Bing / Google / Tavily / Baidu / Ollama / SearXNG / Keenable / Zhipu AI |
| API Integration | Scoped API keys (capability-level grants + per-KB restriction + throttled last-used tracking) with an API integration playground; MCP OAuth and embed sessions isolated per principal |

**Platform**

| Capability | Details |
|------------|---------|
| Deployment | Local / Docker / Kubernetes (Helm) with private and offline support |
| UI | Web UI / RESTful API / Website Embed Widget; the Zilan minimalist design language covers the whole journey |
| Access Control | Workspace RBAC with 4-tier role matrix (Owner / Admin / Contributor / Viewer), per-KB resource ownership, per-workspace audit log, invite-only workspaces, tenantless provisioning & gated self-service workspace creation, admin password reset (session revocation), cross-workspace superuser, scoped API keys |
| Security | AES-256-GCM at-rest encryption for API keys and MCP / data-source credentials with graceful key rotation; gRPC TLS + Token between app and docreader; Redis TLS; SSRF-safe HTTP client (data sources, URL import, redirect chains); secret redaction in responses; sandbox isolation for agent skills |
| Observability | Integrated Langfuse (sole tracing backend) for ReAct loops, token tracking, tool calls, and pipeline tracing; built-in Langfuse-style document parsing trace timeline with stage-by-stage progress; system-admin runtime task-queue dashboard (queue depth, per-model concurrency, failed-task inspection & manual retry) |
| Task Management | MQ async tasks with per-stage worker-pool governance (core / post-process / enrichment / maintenance + elastic shared pool, plus an independent Wiki pool) and per-model background concurrency governors; automatic database migration on version upgrade |
| Model Management | Centralized config, declarative built-in models via YAML, per-knowledge-base model selection, per-model thinking-mode and embedding-dimension overrides, interactive model test debugger, multi-workspace built-in model sharing, Zilan Cloud hosted models and parsing |

## 🚀 Getting Started

### 🛠 Prerequisites

- [Docker](https://www.docker.com/) & [Docker Compose](https://docs.docker.com/compose/)
- [Git](https://git-scm.com/)

### 📦 Installation & Launch

```bash
git clone <your-repository-url> zilan
cd zilan
cp .env.example .env   # Edit .env as needed, see comments in the file
docker compose up -d   # Start core services
```

Once started, visit **http://localhost** to get started.

> To use a local Ollama model, run `ollama serve > /dev/null 2>&1 &` first.

### 🔧 Optional Services (Docker Compose Profiles)

Add `--profile` flags to enable additional components. Multiple profiles can be combined:

| Profile | Description | Command |
|---------|-------------|---------|
| _(default)_ | Core services | `docker compose up -d` |
| `full` | All features | `docker compose --profile full up -d` |
| `neo4j` | Knowledge Graph (Neo4j) | `docker compose --profile neo4j up -d` |
| `minio` | Object Storage (MinIO) | `docker compose --profile minio up -d` |
| `langfuse` | Tracing (Langfuse) | `docker compose --profile langfuse up -d` |

Combine profiles: `docker compose --profile neo4j --profile minio up -d`

Stop services: `docker compose down`

### 🌐 Service URLs

| Service | URL |
|---------|-----|
| Web UI | `http://localhost` |
| Backend API | `http://localhost:8080` |
| Langfuse Tracing | `http://localhost:3000` |

## Knowledge Graph

Zilan converts documents into knowledge graphs that surface how different passages relate to each other. Once enabled, the system analyzes and builds a semantic relation network inside each document — helping users understand the content while providing structural support for indexing and retrieval, improving both relevance and breadth of search results.

See the [Knowledge Graph Configuration Guide](./docs/KnowledgeGraph.md) for setup details.

## MCP Server

Please refer to the [MCP Configuration Guide](./mcp-server/MCP_CONFIG.md) for the necessary setup.

## 📘 Docs

Troubleshooting FAQ: [Troubleshooting FAQ](./docs/QA.md)

Detailed API documentation is available at: [API Docs](./docs/api/README.md)

Product plans and upcoming features: [Roadmap](./docs/ROADMAP.md)

## 🧭 Developer Guide

### ⚡ Fast Development Mode (Recommended)

If you need to frequently modify code, **you don't need to rebuild Docker images every time**! Use fast development mode:

```bash
# Start infrastructure
make dev-start

# Start backend (new terminal)
make dev-app

# Start frontend (new terminal)
make dev-frontend
```

**Development Advantages:**
- ✅ Frontend modifications auto hot-reload (no restart needed)
- ✅ Backend modifications quick restart (5-10 seconds, supports Air hot-reload)
- ✅ No need to rebuild Docker images
- ✅ Support IDE breakpoint debugging

**Detailed Documentation:** [Development Environment Quick Start](./docs/开发指南.md)

## 🤝 Customization Guidelines

Zilan is designed for continuous in-house customization and evolution.

**Process:** Create branch → Commit changes → Merge to mainline

**Standards:** Format code with `gofmt`, follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:` / `fix:` / `docs:` / `test:` / `refactor:`)

## 🔒 Security Notice

For production deployments, we strongly recommend:

- Deploy Zilan services in internal / private network environments rather than the public internet
- Avoid exposing the service directly to public networks to prevent potential information leakage
- Configure proper firewall rules and access controls for your deployment environment
- Regularly update to the latest version for security patches and improvements

## 📄 License

This project is licensed under the [MIT License](./LICENSE).
You are free to use, modify, and distribute the code with proper attribution.
