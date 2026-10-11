# Vigia

Monitor de uptime self-hosted com página de status pública, incidentes automáticos e alertas (Telegram, e-mail, webhook e WhatsApp). É um binário único em Go com PostgreSQL. Com 155 monitores ativos ele roda em cerca de 20 MB de RAM.

## O que tem

| Área | Como funciona |
|---|---|
| Monitores | HTTP(S) (status esperado e palavra-chave), porta TCP, ping ICMP sem root, validade de certificado SSL e heartbeat para cron jobs |
| Motor | Scheduler com worker pool, timeout por checagem, retentativas e regra de N falhas seguidas para não alertar por falso positivo |
| Incidentes | Abertos e fechados automaticamente. O início do incidente é a primeira falha da sequência, não o momento da confirmação |
| Alertas | Interface `Notifier` com Telegram, SMTP, webhook e WhatsApp Cloud API. Alerta ao cair e ao voltar. Enquanto o monitor segue fora, os lembretes saem com backoff exponencial (1h, 2h, 4h… até 24h) |
| Página de status | `/status/{slug}` ou domínio próprio (`status.meusite.com`), barras de 90 dias e histórico de incidentes. A causa aparece "higienizada" para não vazar IP nem host interno |
| Painel | `html/template` + htmx: dashboard com atualização automática, formulário dinâmico por tipo, teste de canal com um clique |
| API | JSON em `/api/v1/...` para tudo que o painel faz |
| Métricas | `/metrics` no formato Prometheus (status, p50/p95, checagens, memória). Tem dashboard pronto para o Grafana |
| SaaS | Contas, sessões, planos Free/Pro com limites, proteção CSRF, CI no GitHub Actions e deploy atrás do Nginx |

## Rodando em 1 minuto

Pré-requisitos: Go 1.27+ e Docker.

```bash
cp .env.example .env
make db-up
make run
```

Abra http://localhost:8080, crie a conta e cadastre um monitor. As migrations rodam sozinhas no start.

Para subir tudo em container (do jeito que vai rodar no homelab):

```bash
make up
make logs
make mem
```

## Testes

```bash
make test
make test-race
make test-integration
```

`make test-integration` sobe um Postgres descartável com testcontainers-go. Se você já tem um Postgres rodando, aponte para ele e cada teste cria e apaga o próprio banco:

```bash
VIGIA_TEST_DATABASE_URL="postgres://vigia:vigia@localhost:5432/postgres?sslmode=disable" make test-integration
```

## Testando na mão com curl

```bash
B=http://localhost:8080

curl -c jar -d "email=eu@exemplo.com&password=senha-forte" $B/signup
curl -b jar $B/api/v1/projects

curl -b jar -H 'Content-Type: application/json' $B/api/v1/monitors -d '{
  "project_id": 1, "name": "Google", "kind": "http",
  "target": "https://www.google.com", "keyword": "Google"
}'

curl -b jar -H 'Content-Type: application/json' $B/api/v1/monitors -d '{
  "project_id": 1, "name": "Porta que não existe", "kind": "tcp",
  "target": "127.0.0.1:9", "failure_threshold": 1, "retries": 0
}'

curl -b jar -H 'Content-Type: application/json' $B/api/v1/monitors -d '{
  "project_id": 1, "name": "Backup noturno", "kind": "heartbeat",
  "interval_seconds": 86400, "timeout_seconds": 1800
}'

curl -b jar $B/api/v1/monitors/1/results?limit=5
curl -b jar $B/api/v1/incidents
curl -X POST $B/hb/<heartbeat_token>
curl $B/metrics
open $B/status/<slug-do-projeto>
```

Para testar a resposta ao SIGTERM, rode `docker compose stop vigia` e olhe os logs: o servidor para de aceitar requisições, termina as checagens em andamento e fecha o pool do banco.

## Comandos do binário

```text
vigia serve                     sobe HTTP + scheduler (padrão)
vigia migrate up|down|status    migrations com goose (embutidas no binário)
vigia set-plan <email> pro      upgrade manual de plano
vigia healthcheck               usado pelo HEALTHCHECK do Docker (distroless não tem curl)
vigia version
```

## Configuração

Tudo vem de variáveis de ambiente (veja `.env.example`). As principais:

| Variável | Padrão | Para quê |
|---|---|---|
| `DATABASE_URL` | obrigatória | conexão Postgres |
| `VIGIA_BASE_URL` | `http://localhost:8080` | links nos alertas e cookie `Secure` automático em https |
| `VIGIA_WORKERS` | `16` | tamanho do worker pool |
| `VIGIA_ALERT_REMINDER` | `1h` | primeiro lembrete enquanto continua fora (`0` desliga) |
| `VIGIA_RESULT_RETENTION` | `720h` | quanto tempo guardar cada checagem bruta |
| `VIGIA_TIMEZONE` | `America/Sao_Paulo` | fuso de exibição (no banco é sempre UTC) |
| `VIGIA_METRICS_TOKEN` | vazio | Bearer token para proteger `/metrics` |
| `TELEGRAM_BOT_TOKEN` | vazio | ativa o canal Telegram |
| `SMTP_*` | vazio | ativa e-mail (587 STARTTLS ou 465 TLS) |
| `WHATSAPP_TOKEN`, `WHATSAPP_PHONE_NUMBER_ID` | vazio | ativa WhatsApp Cloud API |

Um canal sem configuração no servidor aparece desabilitado no painel.

## Deploy no homelab

1. `cp .env.example .env`, trocar `POSTGRES_PASSWORD`, `VIGIA_BASE_URL=https://vigia.seudominio.com` e os tokens.
2. `make up`.
3. Copiar `deploy/nginx/vigia.conf` para `/etc/nginx/sites-available/`, ajustar os domínios, rodar o certbot e recarregar o Nginx.
4. Prometheus: incluir `deploy/prometheus/vigia-scrape.yml` e `alerts.yml`.
5. Grafana: importar `deploy/grafana/vigia-dashboard.json` (ou provisionar com `provisioning-dashboards.yml`).

Para domínio próprio na página de status, o cliente aponta um CNAME para o seu servidor e você adiciona o domínio no `server_name` do Nginx. O Vigia reconhece o `Host` sozinho.

## Estrutura

```text
cmd/vigia            main: subcomandos, wiring e graceful shutdown
internal/config      env → struct validada
internal/store       pgx pool, goose, código gerado pelo sqlc
internal/checker     HTTP, TCP, ping ICMP, SSL, heartbeat
internal/scheduler   agenda + worker pool
internal/incident    máquina de estados, cálculo de uptime, processador
internal/alert       Notifier, dispatcher, anti-spam, canais
internal/monitor     regras de negócio dos monitores, API JSON, heartbeat
internal/auth        senha, sessão, planos
internal/statuspage  página pública
internal/metrics     exposição Prometheus
internal/web         painel htmx, templates, middlewares
```

As decisões de projeto e os conceitos de Go usados estão explicados em [GUIA.md](GUIA.md).
