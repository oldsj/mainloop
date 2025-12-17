# mainloop

A central AI agent orchestrator - one place to manage all AI work, accessible from any device.

Inspired by [You Are The Main Thread](https://claudelog.com/mechanics/you-are-the-main-thread/) - your attention is the bottleneck, so spawn parallel AI processes and never let your cores idle.

## Goals

- **Single Thread** - One continuous conversation across all devices
- **Persistent Memory** - BigQuery stores conversation history and context
- **Mobile Access** - Web UI that works on phone and laptop
- **Claude Code** - Leverage Max subscription via CLI in a container

## Architecture

```
                    ┌─────────────────┐
                    │  Cloudflare     │
                    │  Access + CDN   │
                    └────────┬────────┘
                             │
                    ┌────────▼────────┐
                    │  cloudflared    │
                    │  (K8s pod)      │
                    └────────┬────────┘
                             │
         ┌───────────────────┼───────────────────┐
         │                   │                   │
┌────────▼────────┐ ┌────────▼────────┐ ┌────────▼────────┐
│  Web Frontend   │ │  Backend API    │ │  Claude Code    │
│  (SvelteKit)    │ │  (FastAPI)      │ │  Container      │
└─────────────────┘ └────────┬────────┘ └────────┬────────┘
                             │                   │
                    ┌────────▼────────┐          │
                    │    BigQuery     │◄─────────┘
                    │    (Memory)     │
                    └─────────────────┘
```

## Tech Stack

- **Frontend**: SvelteKit + Tailwind (mobile-first)
- **Backend**: FastAPI (Python)
- **AI**: Claude Code CLI (Max subscription)
- **Database**: BigQuery (conversation memory)
- **Infrastructure**: Home K8s, Cloudflare Tunnel, ArgoCD

## Future

- Voice access via phone (Twilio integration)
- Sub-agent orchestration (spawn K8s jobs for parallel work)
- Structured memory (tasks, decisions, project context)
