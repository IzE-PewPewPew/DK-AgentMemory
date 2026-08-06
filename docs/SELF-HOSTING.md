# Self-hosting

## Docker Compose (recommended)

```bash
git clone https://github.com/IzE-PewPewPew/DK-AgentMemory
cd DK-AgentMemory/deploy
cp .env.example .env
nano .env                    # set POSTGRES_PASSWORD and DKM_PUBLIC_URL
docker compose up -d
docker compose logs dkm | grep -A2 'admin key'
```

The whole stack is configured through `.env`; no config file is mounted, so
there is nothing that can disagree with it.

The admin key prints **once** on first boot. Save it now — it is not recoverable.

```bash
curl -H "Authorization: Bearer <admin-key>" http://localhost:8090/v1/healthz
```

### What comes up

| Service | Port | Exposed |
|---|---|---|
| `dkm` | 8090 | loopback only |
| `postgres` | 5432 | internal network only |
| `embed` | 8091 | internal network only |

Only 8090 needs a route from outside, and it stays on loopback — put a tunnel or reverse proxy in front.

## Native (CentOS / RHEL / Rocky 9)

For hosts already running other services under PM2 or systemd.

### 1. Postgres

```bash
sudo dnf install -y postgresql-server postgresql-contrib
sudo postgresql-setup --initdb
sudo systemctl enable --now postgresql

sudo -u postgres createuser dkm -P
sudo -u postgres createdb -O dkm dkm
sudo -u postgres psql dkm -c 'CREATE EXTENSION IF NOT EXISTS vector;'
sudo -u postgres psql dkm -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'
```

Postgres listens on loopback by default here. Leave it.

If `pgvector` isn't packaged:
```bash
sudo dnf install -y git make gcc postgresql-devel
git clone --branch v0.7.4 https://github.com/pgvector/pgvector
cd pgvector && make && sudo make install
```

### 2. Binary

```bash
sudo mkdir -p /opt/dkm/bin /var/lib/dkm /var/log/dkm
curl -fsSL https://github.com/IzE-PewPewPew/DK-AgentMemory/releases/latest/download/dkm_linux_amd64.tar.gz \
  | sudo tar xz -C /opt/dkm/bin
/opt/dkm/bin/dkm version
```

Before the first tagged release, build it instead:

```bash
git clone https://github.com/IzE-PewPewPew/DK-AgentMemory && cd DK-AgentMemory
make build && sudo install -m 755 bin/dkm /opt/dkm/bin/dkm
```

### 3. Config

```bash
sudo nano /opt/dkm/config.yaml
```

```yaml
server:
  bind: 127.0.0.1:8090
  public_url: https://memories.example.com

database:
  url: postgres://dkm:PASSWORD@127.0.0.1:5432/dkm?sslmode=disable

embedding:
  provider: local
  endpoint: http://127.0.0.1:8091

consolidation:
  enabled: true
  llm:
    provider: anthropic
    model: claude-haiku-4-5
    api_key_env: DKM_LLM_API_KEY

security:
  require_https: true
  rate_limit_writes_per_min: 100
```

```bash
sudo chmod 600 /opt/dkm/config.yaml
```

The server validates this at boot. Unknown keys and missing required keys are fatal, with the offending key named — it will not start half-configured.

### 4. Embedding sidecar

```bash
sudo dnf install -y python3 python3-pip
sudo mkdir -p /opt/dkm/embed
sudo cp deploy/embed/app.py deploy/embed/requirements.txt /opt/dkm/embed/
sudo python3 -m venv /opt/dkm/embed/venv
sudo /opt/dkm/embed/venv/bin/pip install -r /opt/dkm/embed/requirements.txt
```

The sidecar downloads its model on first start, which takes a few minutes on a
small VPS. The API runs without it — search is keyword-only until it is ready,
and the backfill pass vectorises anything written in the meantime.

### 5. Migrate and start

```bash
sudo /opt/dkm/bin/dkm migrate --config /opt/dkm/config.yaml
```

**PM2** — note the explicit `PATH`, `HOME`, and `cwd`; PM2 inherits none of your login shell:

```javascript
// /opt/dkm/ecosystem.config.js
module.exports = {
  apps: [
    {
      name: 'dkm',
      script: '/opt/dkm/bin/dkm',
      args: 'serve --config /opt/dkm/config.yaml',
      cwd: '/opt/dkm',
      interpreter: 'none',
      env: {
        PATH: '/usr/local/bin:/usr/bin:/bin',
        HOME: '/opt/dkm',
        DKM_LLM_API_KEY: 'sk-ant-...'
      },
      autorestart: true,
      min_uptime: '30s',
      kill_timeout: 10000,
      error_file: '/var/log/dkm/err.log',
      out_file: '/var/log/dkm/out.log',
      merge_logs: true,
      time: true
    },
    {
      name: 'dkm-embed',
      script: '/opt/dkm/embed/venv/bin/python',
      args: '-m uvicorn app:api --host 127.0.0.1 --port 8091',
      cwd: '/opt/dkm/embed',
      interpreter: 'none',
      autorestart: true,
      min_uptime: '30s'
    }
  ]
};
```

```bash
pm2 start /opt/dkm/ecosystem.config.js
pm2 save
pm2 startup systemd -u root --hp /root   # run the printed command
```

`pm2 save` snapshots every app under PM2. Confirm the rest of your fleet is healthy first.

**systemd** — unit files in `deploy/systemd/`.

### 6. Verify the bind address

```bash
sudo ss -tlnp | grep 8090
```

Must show `127.0.0.1`. If it shows `0.0.0.0`, the API is on your public interface and bypassing whatever you put in front of it.

## Exposing it

### Cloudflare Tunnel

```yaml
ingress:
  - hostname: memories.example.com
    service: http://localhost:8090
  - service: http_status:404
```

```bash
cloudflared tunnel route dns <tunnel-name> memories.example.com
cloudflared tunnel ingress validate
sudo systemctl restart cloudflared    # or `pm2 restart <name>` if PM2-managed
```

The viewer is served from the same origin under `/viewer` — no second hostname, no WebSocket path rewriting. Live updates use SSE, which tunnels as plain HTTP.

### Caddy

```
memories.example.com {
    reverse_proxy 127.0.0.1:8090
}
```

## First user

```bash
export DKM_ADMIN_KEY=<admin key from first boot>

dkm admin team create --id acme --name "Acme Engineering"
dkm admin user create --team acme --id kuong --name "Kuong"
dkm admin key issue --user kuong --label "laptop"
# → pmk_a3f2_xxxxxxxx   (shown once)
```

Give each person their own key. Revoking one person is then a single operation with no impact on anyone else:

```bash
dkm admin key revoke <id>
```

## Backup

```bash
pg_dump -Fc dkm > /var/backups/dkm-$(date +%F).dump
```

Nightly:
```
0 3 * * * pg_dump -Fc dkm > /var/backups/dkm-$(date +\%F).dump && \
          find /var/backups -name 'dkm-*.dump' -mtime +14 -delete
```

Restore:
```bash
pg_restore -d dkm --clean /var/backups/dkm-2026-08-05.dump
```

Test a restore into a scratch database before you need one.

## Upgrade

```bash
pm2 stop dkm
curl -fsSL <release-url> | sudo tar xz -C /opt/dkm/bin
sudo /opt/dkm/bin/dkm migrate --config /opt/dkm/config.yaml
pm2 start dkm
```

Migrations are forward-only and idempotent. Back up first anyway.

## Monitoring

```
GET /v1/livez     no auth, liveness
GET /v1/healthz   auth, deep check
GET /metrics      Prometheus, loopback only
```

Point uptime checks at `/v1/livez`.
