# Guia do Vigia: decisões e Go para quem vem de PHP/Java

O código não tem comentários, como você pediu. A explicação do porquê de cada escolha fica aqui. A ordem segue o caminho de uma checagem, do start do processo até o alerta chegar no celular.

---

## 1. O fluxo inteiro em uma figura

```text
               ┌────────────── main (errgroup) ───────────────┐
               │                                              │
  SIGTERM ──▶ ctx cancelado                                   │
               │                                              │
   ┌───────────▼──────────┐   jobs (chan int64)   ┌───────────┴─────────┐
   │ scheduler.Run        │ ────────────────────▶ │ N workers           │
   │ map[id]*entry        │ ◀──────────────────── │ processor.Run(id)   │
   │ ticker 1s / reload   │   done (chan int64)   └──────────┬──────────┘
   └──────────────────────┘                                  │
                                         checker.Check ──────┤ (HTTP/TCP/ping/SSL/heartbeat + retries)
                                                             │
                                  ┌──────────────── tx ──────▼──────────────────┐
                                  │ INSERT check_results                       │
                                  │ Evaluate(estado, ok, threshold)            │
                                  │ UPDATE monitors (status, falhas, desde)    │
                                  │ OpenIncident / ResolveIncident             │
                                  └──────────────────────┬─────────────────────┘
                                                         │ depois do commit
                                          alert.Dispatcher.Notify (errgroup, limite 4)
                                          ├─ Telegram
                                          ├─ E-mail
                                          ├─ Webhook
                                          └─ WhatsApp
```

---

## 2. Decisões de arquitetura

**Uptime calculado a partir dos incidentes, não das checagens.** Se o uptime fosse "checagens ok / total", um soluço isolado derrubaria a porcentagem mesmo sem incidente, e a página de status ia contradizer a regra de N falhas. Com o cálculo em cima dos intervalos de incidente (`incident.Downtime` funde intervalos sobrepostos e recorta na janela), o número reflete a queda confirmada. Outra vantagem: o histórico bruto de checagens pode ser apagado depois de 30 dias sem perder o uptime de 90 dias, porque os incidentes ficam guardados para sempre e ocupam pouquíssimo espaço.

**O incidente começa na primeira falha.** O estado guarda `failing_since`. Quando a terceira falha confirma a queda, o incidente é aberto com `started_at` igual à primeira. Assim a duração mostrada é a real.

**Duas camadas contra falso positivo.** As `retries` tentam de novo dentro da mesma checagem, com intervalo de 2s. O `failure_threshold` exige N checagens seguidas com falha. A primeira camada pega a perda de pacote; a segunda pega a instabilidade de alguns segundos.

**Anti-spam.** Cair e voltar geram um alerta cada, porque só a transição de estado dispara alerta. Enquanto o monitor continua fora, `alert.ShouldRemind` usa backoff exponencial (1h, 2h, 4h, 8h, 16h e depois 24h). O processador marca `last_alert_at` mesmo quando algum canal falha, senão um Telegram fora do ar faria o Vigia tentar de novo a cada checagem.

**Alerta fora da transação.** Os dados são gravados numa transação curta. A chamada HTTP para o Telegram acontece depois do commit. Segurar uma conexão do pool enquanto se espera uma API externa é um jeito clássico de esgotar o pool.

**Uma checagem por monitor por vez.** O scheduler mantém um `busy[id]`. Se uma checagem demora mais que o intervalo (timeout de 10s, intervalo de 10s e mais retries), a próxima não é disparada em paralelo. Isso evita duas goroutines brigando pelo mesmo estado.

**Envio de job sem bloquear.** O scheduler usa `select { case jobs <- id: ... default: }`. Se todos os workers estão ocupados, ele simplesmente tenta no próximo tick, sem travar o loop principal (que também precisa ler `done`, recarregar e responder ao shutdown).

**Recarga híbrida.** Criar ou editar um monitor chama `sched.Reload()` (um canal com buffer 1 e envio não bloqueante, então dez cliques viram uma única recarga). Além disso, a cada 30s a lista é relida do banco, para pegar alterações feitas por outra instância ou direto no SQL.

**Jitter no start.** Sem ele, 200 monitores disparam no mesmo segundo depois de um restart. Cada monitor ganha até 5s de atraso aleatório, e se a última checagem foi recente o monitor espera o intervalo normal.

**Shutdown gracioso de verdade.** Os workers recebem `context.WithoutCancel(ctx)`: o SIGTERM para o scheduler de *distribuir* trabalho, mas não cancela a checagem em andamento (cada uma já tem o próprio timeout). A ordem é: o HTTP para de aceitar requisições, o scheduler fecha `jobs`, espera os workers com `WaitGroup`, e só no fim o `defer db.Close()` fecha o pool. Existe um teste para isso (`TestShutdownWaitsForInFlightChecks`).

**Ping sem root.** No Linux, `ping_linux.go` abre um socket `SOCK_DGRAM` + `IPPROTO_ICMP`, que o kernel permite para grupos em `net.ipv4.ping_group_range`. O `compose.yaml` libera isso, então o container roda como `nonroot` no distroless sem `CAP_NET_RAW`. Em outros sistemas, `ping_other.go` cai no socket raw. O sufixo `_linux.go` no nome do arquivo já é uma build constraint: Go compila cada arquivo só no sistema certo.

**`/metrics` escrito à mão.** O `client_golang` traria umas 15 dependências para meia dúzia de métricas. O formato de exposição do Prometheus é texto simples e o teste garante o escape de labels. Se um dia as métricas crescerem muito, trocar é fácil porque fica tudo isolado em `internal/metrics`.

**Página de status sem vazar nada.** O alvo (URL, IP) nunca aparece na página pública, e a causa passa por `publicCause` ("connection refused 10.0.0.5:5432" vira "Serviço indisponível").

**CSRF com a stdlib.** O Go 1.25 trouxe `http.NewCrossOriginProtection`, que usa `Sec-Fetch-Site`/`Origin` para barrar POST vindo de outro site. Não precisa de token em formulário. Requisições sem esses headers (curl, cron batendo no heartbeat) passam normalmente.

**Sessão com token opaco no banco, não JWT.** Para dar logout você só apaga a linha da sessão. No banco fica o SHA-256 do token, então um dump vazado não vira sessão válida.

**Horário.** O Postgres grava `timestamptz` (UTC internamente). A sessão é forçada para `timezone=UTC` e o codec do pgx lê em UTC. O fuso de São Paulo só entra na hora de exibir (`web.Funcs`) e de formatar alerta. Com `import _ "time/tzdata"`, a base de fusos vai embutida no binário, porque a imagem distroless não tem `/usr/share/zoneinfo`.

**Pacotes `web` e `metrics` além da lista original.** Eles cuidam de entrega (HTML e Prometheus), não de domínio. Colocar o painel dentro de `monitor` ia misturar regra de negócio com template.

---

## 3. Conceitos de Go que aparecem no código

### Interfaces implícitas, definidas por quem consome

Em Java você escreve `class Telegram implements Notifier`. Em Go não existe `implements`: qualquer tipo que tenha os métodos satisfaz a interface automaticamente.

```go
type Notifier interface {
	Send(ctx context.Context, target string, msg Message) error
}
```

`*Telegram`, `*Email`, `*Webhook` e `*WhatsApp` não sabem que essa interface existe. Isso permite que **quem usa** declare só o que precisa. Repare em `scheduler.Source`: ela pede só `ListEnabledMonitors`. O `*store.DB` tem 40 métodos e satisfaz a interface, e no teste um `fakeSource` de 10 linhas também satisfaz. Não precisa de Mockito.

Regra prática: interface pequena, no pacote que consome, e só quando existe mais de uma implementação (mesmo que a segunda seja o fake do teste).

### Ponteiro vs valor

```go
func (s *Scheduler) Reload()
func (k Kind) Valid() bool
```

O receiver com `*` é como `this` em Java: ele altera o objeto original. Sem `*`, o método recebe uma **cópia**. Use ponteiro quando o método muda estado ou quando a struct é grande ou tem `sync.Mutex` (mutex nunca pode ser copiado). Use valor para tipos pequenos e imutáveis, como `Kind`, `State` e `Interval`.

O sqlc gera campos `*time.Time` e `*string` para colunas `NULL`: `nil` é o `NULL` do banco. Por isso existe `derefBool` no template e os `if m.LastHeartbeatAt != nil`.

### Erros são valores

Não existe `try/catch`. A função devolve `(resultado, error)` e quem chama decide o que fazer:

```go
m, err := s.db.GetMonitorForUser(ctx, params)
if err != nil {
	if store.IsNotFound(err) {
		return store.Monitor{}, ErrNotFound
	}
	return store.Monitor{}, fmt.Errorf("buscar monitor: %w", err)
}
```

O `%w` embrulha o erro original, e lá em cima `errors.Is(err, ErrNotFound)` ou `errors.As(err, &verrs)` continuam funcionando através das camadas. É o equivalente a encadear `cause` numa exception, só que explícito. `ValidationErrors` é um `map` que implementa `Error()`, então dá para devolvê-lo como `error` e a API recupera os campos com `errors.As`.

### Goroutines e channels

Goroutine é uma "thread" de uns 2 KB gerenciada pelo runtime. Você pode criar milhares delas. Channel é uma fila tipada e thread-safe entre goroutines:

```go
jobs := make(chan int64)
done := make(chan int64, s.opts.Workers)
```

Sem buffer, o envio só completa quando alguém recebe (é um encontro marcado). Com buffer N, até N envios entram sem esperar. O worker pool inteiro são 16 goroutines fazendo `for id := range jobs`. Quando o scheduler faz `close(jobs)`, o `range` termina e o worker sai. Isso substitui o `ExecutorService` do Java com bem menos código.

O `select` espera vários canais ao mesmo tempo. É o coração do `scheduler.Run`: tick, resultado de worker, reload, trigger de heartbeat e shutdown, todos num lugar só, sem lock, porque só essa goroutine mexe no `map entries`. Em Go o lema é: *não comunique compartilhando memória; compartilhe memória comunicando*.

### `sync.WaitGroup`, `errgroup` e `-race`

`wg.Go(func() {...})` (Go 1.25) dispara a goroutine e conta. `wg.Wait()` bloqueia até todas terminarem. O `errgroup` faz o mesmo e ainda devolve o primeiro erro e cancela um contexto compartilhado. O `main` usa um errgroup para HTTP, shutdown, scheduler e limpeza: se o HTTP não consegue abrir a porta, todo o resto para junto.

`go test -race` instrumenta cada acesso à memória e acusa duas goroutines tocando o mesmo dado sem sincronização. Todos os testes passam com ele.

### `context.Context`

É o "cancelamento + prazo + valores" que atravessa toda chamada de I/O. Em PHP o request morre e leva tudo junto. Em Go o servidor vive para sempre, então cada operação precisa saber quando desistir:

- `context.WithTimeout(ctx, timeout)` em cada checagem
- `signal.NotifyContext` transforma SIGTERM em cancelamento
- `context.WithoutCancel(ctx)` mantém os valores e ignora o cancelamento (usado nos workers)
- `context.AfterFunc(ctx, conn.Close)` no ping, para destravar um `ReadFrom` bloqueado

Regra: `ctx` é sempre o primeiro parâmetro e nunca fica guardado dentro de uma struct.

### `defer`

```go
resp, err := c.HTTPClient.Do(req)
if err != nil { ... }
defer resp.Body.Close()
```

Executa quando a função retorna, por qualquer caminho. É o `finally` do Java, só que escrito logo ao lado de onde o recurso foi aberto. Em `InTx`, o `defer tx.Rollback(...)` depois do commit não faz nada, e se der erro no meio desfaz tudo.

### Composição em vez de herança

```go
type DB struct {
	*Queries
	Pool *pgxpool.Pool
}
```

Isso é *embedding*: `db.GetMonitor(...)` funciona porque os métodos de `*Queries` são "promovidos" para `DB`. Parece herança, mas não é: `DB` não "é um" `Queries`, ele *contém* um. O `statusRecorder` do middleware faz o mesmo com `http.ResponseWriter`, sobrescrevendo só `WriteHeader` e `Write`.

### Loop var e generics leves

Desde o Go 1.22 cada iteração do `for` tem a própria variável. Por isso `for _, ch := range chans { g.Go(func() { d.send(ctx, ch, msg) }) }` é seguro. Em versões antigas, todas as goroutines viam o último `ch`. Também aparecem `min`/`max` embutidos, `for i := range 5`, `slices.SortFunc` e `slices.Contains`.

### `embed`

`//go:embed templates` coloca os arquivos dentro do binário em tempo de compilação. Migrations, templates, CSS e htmx viajam juntos: uma imagem, um arquivo, nada para montar em volume.

### Testes table-driven

```go
tests := []struct{ name string; in X; want Y }{ ... }
for _, tt := range tests {
	t.Run(tt.name, func(t *testing.T) { ... })
}
```

Cada linha da tabela é um caso, e `t.Run` dá nome para rodar só um (`go test -run 'TestEvaluate/caiu'`). Os testes críticos estão em `incident/state_test.go` (falso positivo), `incident/uptime_test.go` (cálculo e virada de dia no fuso), `alert/alert_test.go` (anti-spam) e `incident/processor_integration_test.go` (ciclo completo contra Postgres real).

---

## 4. Onde estamos no roadmap

| Fase | Status | O que foi entregue |
|---|---|---|
| 1. Fundação | ✅ | módulo, config por env, slog JSON/text, Compose, goose embutido, sqlc, `/healthz` e `/readyz`, graceful shutdown |
| 2. Motor | ✅ | CRUD (painel e API), scheduler + pool, resultados, regra de N falhas, retries |
| 3. Incidentes e alertas | ✅ | abertura e fechamento automáticos, Telegram, e-mail, webhook e WhatsApp, anti-spam com backoff, botão "Testar" |
| 4. Página de status | ✅ | painel htmx, status pública com 90 dias, domínio próprio |
| 5. Mais monitores | ✅ | TCP, ping, SSL, heartbeat, p50/p95, `/metrics`, dashboard Grafana, regras de alerta do Prometheus |
| 6. SaaS | 🟡 | auth, multiusuário, planos e limites, CI com GHCR, Nginx. **Faltam:** cobrança automática, reset de senha por e-mail, rate limit no login |

### Medições (máquina de teste, Postgres 18 local)

- 155 monitores a cada 30s (HTTP, TCP e ping): **~20,5 MB de RSS**, 26 goroutines, heap de 3–4 MB, cerca de 300 checagens por minuto, zero erros.
- Binário estático: 16 MB com `-s -w`.
- `go test -race -tags integration ./...`: tudo verde.

---

## 5. Próximos passos sugeridos

1. **Rate limit no login e no signup**: um `map[ip]*bucket` com `sync.Mutex` e limpeza periódica. Dá para fazer sem dependência e é um ótimo exercício de concorrência.
2. **Reset de senha** reaproveitando o `alert.Email` e uma tabela `password_resets` com token hasheado, igual às sessões.
3. **Chave de API por usuário** (`Authorization: Bearer`) para automatizar sem cookie.
4. **Checagem de múltiplas regiões**: um segundo binário "agente" que só roda o `checker` e reporta para o principal. Isso reduz falso positivo quando o problema é a rede do homelab.
5. **Cobrança** (Stripe ou Mercado Pago) chamando `SetUserPlan` a partir de webhook.

Uma observação honesta: o `WhatsApp Cloud API` só deixa mandar texto livre dentro da janela de 24h de conversa. Para alertas em produção você vai precisar de um *template* aprovado pela Meta. O canal já está pronto e a troca é só no payload.
