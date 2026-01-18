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

O binário **não** carrega o arquivo `.env` automaticamente. É necessário exportar as variáveis antes de rodar.

Exemplos:

```bash
set -a; source .env; set +a; ./univiewd run
```

Ou:

```bash
env $(grep -v '^#' .env | xargs) ./univiewd run
```

### Autenticação / conexão

- `UNV_BASE_URL`: base URL da câmera (ex.: `http://192.168.1.10`)
- `UNV_USER`: usuário
- `UNV_PASS`: senha

### Receiver

- `RECEIVER_HOST`: host do listener (default `0.0.0.0`)
- `RECEIVER_PORT`: porta do listener (default `8080`)
- `EVENT_TAG`: tag do payload normalizado (default `uniview`)
- `EVENT_CATEGORY`: categoria do payload normalizado (default `event`)

#### Normalização de eventos (forwarding)

O receiver monta um payload normalizado com os campos `tag`, `categoria`, `camera_ip`, `ivs_type`, `message`.

- `camera_ip` é extraído na seguinte ordem: `X-Forwarded-For` (primeiro IP), `X-Real-IP`, `req.RemoteAddr`.
- `ALARM_TYPE_MAPPING_JSON`: JSON com mapeamento de `AlarmType` → `{ "ivs_type": "...", "message": "..." }`.
- `ALARM_TYPE_MAPPING_FILE`: caminho para arquivo JSON com o mesmo formato (tem precedência sobre o env).

### Forwarding (encaminhar eventos recebidos)

- `FORWARD_URL`: URL completa para POST (ex.: `http://localhost:9000/webhooks/uniview`)
- `FORWARD_SCHEME`: esquema (default `http`)
- `FORWARD_HOST`: host do destino (ex.: `localhost` ou `10.0.0.5`)
- `FORWARD_PORT`: porta do destino (opcional)
- `FORWARD_PATH`: path do destino (default `/`)

> Use `FORWARD_URL` **ou** `FORWARD_HOST` + `FORWARD_PATH` (com `FORWARD_SCHEME`/`FORWARD_PORT` se necessário).

### Subscription / keepalive

- `DURATION`: duração da subscription (segundos)
- `TYPE_MASK`: máscara de eventos (ex.: `97663` **se aplicável no PDF**)
- `IMAGE_PUSH_MODE`: modo de push de imagens (se aplicável no PDF)
- `SUBSCRIPTION_ID`: id para keepalive/unsubscribe (quando necessário)

> ⚠️ **Aviso**: `RECEIVER_HOST` deve ser o IP/host acessível pela câmera (não `0.0.0.0`). Em cenários com NAT, IP público, VPN ou reverse proxy, use o endereço exposto para a câmera alcançar o callback.

### Payloads obrigatórios (não inventamos campos)

Os endpoints exigem payloads JSON **conforme o PDF**. Para evitar inventar campos, o binário usa templates fornecidos pelo operador.

- `SUBSCRIBE_PAYLOAD` **ou** `SUBSCRIBE_PAYLOAD_FILE`
- `KEEPALIVE_PAYLOAD` **ou** `KEEPALIVE_PAYLOAD_FILE`

Templates suportam placeholders:

- `{{CALLBACK_URL}}`
- `{{DURATION}}`
- `{{TYPE_MASK}}`
- `{{IMAGE_PUSH_MODE}}`
- `{{SUBSCRIPTION_ID}}`

Veja `examples/subscribe_payload_template.json` e `examples/keepalive_payload_template.json` para referência de placeholders. Substitua os nomes dos campos pelos definidos no PDF.

## Como rodar

### 1) Iniciar receiver

```bash
RECEIVER_HOST=0.0.0.0 RECEIVER_PORT=8080 ./univiewd serve
```

- Receiver expõe métricas simples em `/debug/vars`.

Exemplo com forwarding:

```bash
export FORWARD_URL=http://localhost:9000/webhooks/uniview
export EVENT_TAG=uniview
export EVENT_CATEGORY=event
export ALARM_TYPE_MAPPING_JSON='{"Motion":{"ivs_type":"motion","message":"Motion detected"}}'
RECEIVER_HOST=0.0.0.0 RECEIVER_PORT=8080 ./univiewd serve
```

### 2) Criar subscription

```bash
export UNV_BASE_URL=http://192.168.1.10
export UNV_USER=admin
export UNV_PASS=secret
export RECEIVER_HOST=192.168.1.100
export RECEIVER_PORT=8080
export DURATION=60
export TYPE_MASK=97663
export IMAGE_PUSH_MODE=0
export SUBSCRIBE_PAYLOAD_FILE=examples/subscribe_payload_template.json

./univiewd subscribe
```

### 3) Keepalive

```bash
export SUBSCRIPTION_ID=<id-retornado>
export KEEPALIVE_PAYLOAD_FILE=examples/keepalive_payload_template.json

./univiewd keepalive
```

### 4) Rodar tudo em modo daemon

```bash
export CAMERA_CSV_FILE=examples/cameras.csv
export SUBSCRIBE_PAYLOAD_FILE=examples/subscribe_payload_template.json
export KEEPALIVE_PAYLOAD_FILE=examples/keepalive_payload_template.json
export RECEIVER_HOST=192.168.1.100
export RECEIVER_PORT=8080
export DURATION=60
export TYPE_MASK=97663
export IMAGE_PUSH_MODE=0
export FORWARD_HOST=localhost
export FORWARD_PORT=9000
export FORWARD_PATH=/webhooks/uniview
# ou, alternativamente:
# export FORWARD_URL=http://localhost:9000/webhooks/uniview

./univiewd run
```

> Observação: o callback URL final será `http://RECEIVER_HOST:RECEIVER_PORT/LAPI/V1.0/System/Event/Notification`.

## Exemplos de payloads e eventos

- Template de subscription: `examples/subscribe_payload_template.json`
- Template de keepalive: `examples/keepalive_payload_template.json`
- Exemplo de evento recebido: `examples/notification_event.json`
- ACK esperado: `examples/ack.json`

### Exemplo com curl (receiver)

```bash
curl -X POST http://localhost:8080/LAPI/V1.0/System/Event/Notification/1 \
  -H 'Content-Type: application/json' \
  -d @examples/notification_event.json
```

## CSV de câmeras

Suporte a CSV com formato:

```
<ip>,<porta>,<login>,<senha>,<modelo>
```

- Se `modelo` estiver vazio, assume `uniview`.
- Exemplo em `examples/cameras.csv`.

Para rodar o daemon com múltiplas câmeras, defina `CAMERA_CSV_FILE` com o caminho do CSV.
Quando definido, o comando `run` ignora `UNV_BASE_URL`, `UNV_USER` e `UNV_PASS` e abre
uma subscription/keepalive por câmera (cada linha do CSV cria um client próprio).

Exemplo:

```bash
export CAMERA_CSV_FILE=examples/cameras.csv
export SUBSCRIBE_PAYLOAD_FILE=examples/subscribe_payload_template.json
export KEEPALIVE_PAYLOAD_FILE=examples/keepalive_payload_template.json
export FORWARD_HOST=localhost
export FORWARD_PORT=9000
export FORWARD_PATH=/webhooks/uniview

./univiewd run
```

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
