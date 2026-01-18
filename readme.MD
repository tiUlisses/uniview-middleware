Informações sobre a API estão nos PDFs constantes no projeto



# Uniview LiteAPI (Go) — Subscription + Notifications + Analytics

Integração em Go para câmeras Uniview (LiteAPI / LAPI), cobrindo:
- Autenticação **HTTP Digest**
- **Subscription** de eventos (alarms/analytics)
- **Keepalive** da subscription
- **Receiver HTTP** para notificações (push da câmera)
- ACK de eventos conforme esperado pela câmera
- Suporte a assinar **tudo** (bitmask) ou **categorias específicas**
- Estrutura pronta para capturar e normalizar “analíticos” (AlarmType / paths)

> Referência principal: `docs/LiteAPI Document for IPC V5.04.pdf`

---

## Estrutura do repositório

