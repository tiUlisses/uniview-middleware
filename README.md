# Uniview LiteAPI (Go) — Subscription + Notifications + Analytics

Integração em Go para câmeras Uniview (LiteAPI / LAPI), cobrindo:

- Autenticação **HTTP Digest** (RFC 2617)
- **Subscription** de eventos (alarms/analytics)
- **Keepalive** da subscription
- **Receiver HTTP** para notificações (push da câmera) com ACK JSON
- Suporte para assinar **tudo** (bitmask) ou **categorias específicas** via `TYPE_MASK`
- Componentes modulares (client outbound e receiver inbound)

> Referência principal: `docs/LiteAPI Document for IPC V5.04.pdf` (obrigatória para validar schema e campos)

## Estrutura do repositório

```
cmd/univiewd               # Binário CLI/daemon
pkg/uniview/client         # Cliente LiteAPI (outbound)
pkg/uniview/digest         # HTTP Digest transport
pkg/uniview/receiver       # Receiver HTTP (inbound)
examples/                  # Payloads de exemplo + CSV
```

## Requisitos

- Go 1.21+
- Credenciais via env vars (não commitar usuários/senhas)

## Configuração por variáveis de ambiente

### Carregar .env

O binário tenta carregar automaticamente um arquivo `.env` no diretório atual antes de ler as variáveis. Se o arquivo não existir, ele segue normalmente sem erro. Variáveis já definidas no ambiente têm prioridade.

Se `ENV_FILE` estiver definido, o caminho apontado será carregado **após** o `.env` e sobrescreve seus valores. Isso permite ter múltiplos arquivos de configuração sem alterar o conteúdo do `.env` padrão.

O repositório inclui um `.env.example` com todas as variáveis suportadas e exemplos seguros. Copie para `.env` e ajuste conforme necessário.

Exemplo:

```bash
./univiewd run
```

### Autenticação / conexão

- `UNV_BASE_URL`: base URL da câmera (ex.: `http://192.168.1.10`)
- `UNV_USER`: usuário
- `UNV_PASS`: senha

### Receiver

- `RECEIVER_LISTEN_HOST`: host do listener/bind (default `0.0.0.0`)
- `RECEIVER_CALLBACK_HOST`: host/hostname/IP que a câmera usa para chamar o callback (opcional; se vazio, detecta automaticamente o IP local usado para alcançar a câmera)
- `RECEIVER_PORT`: porta do listener (default `8080`)
- `EVENT_TAG`: tag do payload normalizado (default `uniview`)
- `EVENT_CATEGORY`: categoria do payload normalizado (default `event`)

#### Normalização de eventos (forwarding)

O receiver monta um payload normalizado com os campos `tag`, `categoria`, `camera_ip`, `ivs_type`, `message`.

- `camera_ip` é extraído na seguinte ordem: `X-Forwarded-For` (primeiro IP), `X-Real-IP`, `req.RemoteAddr`.
- `ALARM_TYPE_MAPPING_JSON`: JSON com mapeamento de `AlarmType` → `{ "ivs_type": "...", "message": "..." }`.
- `ALARM_TYPE_MAPPING_FILE`: caminho para arquivo JSON com o mesmo formato (tem precedência sobre o env).

### Forwarding (encaminhar eventos recebidos)

- `ANALYTICS_HOST`: host do destino (ex.: `localhost` ou `10.0.0.5`)
- `ANALYTICS_PORT`: porta do destino
- `ANALYTICS_PATH`: path do destino (endpoint)

> Observação: as chaves antigas `FORWARD_*` continuam aceitas por compatibilidade, mas o fluxo recomendado é usar `ANALYTICS_*`.

### Subscription / keepalive

- `DURATION`: duração da subscription (segundos)
- `TYPE_MASK`: máscara de eventos (ex.: `97663` **se aplicável no PDF**)
- `IMAGE_PUSH_MODE`: modo de push de imagens (se aplicável no PDF)
- `SUBSCRIPTION_ID`: id para keepalive/unsubscribe (quando necessário)

> ⚠️ **Aviso**: `RECEIVER_CALLBACK_HOST` deve ser o IP/host acessível pela câmera (não `0.0.0.0`). Em cenários com NAT, IP público, VPN ou reverse proxy, use o endereço exposto para a câmera alcançar o callback. Se não definir, o serviço tenta detectar automaticamente o IP local usado para alcançar a câmera.

## Como rodar (guia único)

Fluxo recomendado: **`.env` → CSV → `run`**. O daemon lê o `.env`, carrega o CSV com as câmeras e cria **um worker por linha** para manter subscriptions ativas e enviar eventos para o servidor configurado no `.env`.

### 1) `.env`

Crie/ajuste seu `.env` (ou use `ENV_FILE`) com as variáveis usadas pelo daemon e pelo receiver:

```dotenv
# Listener/receiver
RECEIVER_LISTEN_HOST=0.0.0.0
# Callback que a câmera alcança (opcional; se vazio, detecta IP local)
RECEIVER_CALLBACK_HOST=192.168.1.100
RECEIVER_PORT=8080

# Subscription / keepalive
DURATION=60
TYPE_MASK=97663
IMAGE_PUSH_MODE=0

# Forwarding (destino analytics)
ANALYTICS_HOST=analytics.local
ANALYTICS_PORT=9000
ANALYTICS_PATH=/webhooks/uniview
```

> ⚠️ **Importante**: o `RECEIVER_CALLBACK_HOST` deve ser acessível pela câmera (não use `0.0.0.0` como callback). Se não definir, o serviço tenta detectar automaticamente o IP local usado para alcançar a câmera.

### 2) CSV

Crie o CSV de câmeras (uma linha por câmera):

```bash
cat > examples/cameras.csv <<'CSV'
192.168.1.10,80,admin,secret,uniview
192.168.1.11,80,admin,secret,uniview
192.168.1.12,80,admin,secret,uniview
192.168.1.13,80,admin,secret,uniview
192.168.1.14,80,admin,secret,uniview
CSV
```

O formato é:

```
<ip>,<porta>,<login>,<senha>,<modelo>
```

Se `modelo` estiver vazio, assume `uniview`.

### 3) `run`

Execute o daemon apontando para o CSV:

```bash
export CAMERA_CSV_FILE=examples/cameras.csv
./univiewd run
```

#### O que acontece no `run`

- O processo **carrega o `.env`** (e `ENV_FILE`, se definido).
- O **CSV define o caminho e as credenciais**; **cada linha gera um worker** dedicado.
- Cada worker executa o ciclo **subscribe → keepalive → resubscribe** continuamente (keepalive automático).
- O receiver **normaliza e envia** os eventos recebidos para o servidor configurado em `ANALYTICS_*`.

## Supervisor / Workers

O comando `run` inicia um supervisor que cria **um worker por câmera**. Cada worker roda em loop contínuo de **subscribe → keepalive → resubscribe**, tentando manter a subscription ativa enquanto o processo estiver vivo.

### Variáveis do supervisor

As variáveis abaixo controlam o comportamento do supervisor/worker. Valores de duração aceitam o formato do `time.ParseDuration` (ex.: `15s`, `2m`) ou um inteiro em **segundos**.

- `WORKER_MAX_CONCURRENCY`: máximo de workers em paralelo (int). Default: `4`.
- `SUBSCRIBE_RETRY_BACKOFF`: espera antes de tentar resubscribe após falha (duration). Default: `15s`.
- `KEEPALIVE_JITTER`: jitter adicionado ao intervalo de keepalive (duration). Default: `2s`.
- `WORKER_SHUTDOWN_TIMEOUT`: tempo máximo para shutdown gracioso (duration). Default: `10s`.
- `MAX_KEEPALIVE_FAILURES` (ou `KEEPALIVE_MAX_FAILURES`): número de falhas consecutivas de keepalive antes de resubscrever (int). Default: `3`.
- `KEEPALIVE_BACKOFF_BASE`: base do backoff em falhas de keepalive (duration). Default: `2s`.
- `KEEPALIVE_BACKOFF_MAX`: limite máximo do backoff (duration). Default: `30s`.

> Observação: o callback URL final será `http://RECEIVER_CALLBACK_HOST:RECEIVER_PORT/LAPI/V1.0/System/Event/Notification` (ou o IP local detectado automaticamente quando `RECEIVER_CALLBACK_HOST` estiver vazio).

## CSV de câmeras

Suporte a CSV com formato:

```
<ip>,<porta>,<login>,<senha>,<modelo>
```

- Se `modelo` estiver vazio, assume `uniview`.
- Exemplo em `examples/cameras.csv` com 5 entradas; cada linha gera um worker próprio.

Para rodar o daemon com múltiplas câmeras, defina `CAMERA_CSV_FILE` com o caminho do CSV.
Quando definido, o comando `run` abre uma subscription/keepalive por câmera (cada linha do CSV cria um client próprio).

## Observabilidade

- Logs com ciclo subscribe/keepalive e recebimento de eventos.
- Métricas básicas via `expvar` (`/debug/vars`).

## Testes

```bash
go test ./...
```

## Notas importantes

- **Não inventar campos**: use o PDF `docs/LiteAPI Document for IPC V5.04.pdf` para definir os nomes dos campos corretos.
- **Segurança**: não commite credenciais. Use `.env` (gitignored) ou env vars.
