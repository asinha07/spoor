# telespoor.com

The project website. A single static page in [`public/`](public/), no build step.

## Edit

Open `public/index.html` in a browser, change it, commit. The live site picks up `main` within about 10 minutes.

## How it is served

A host clones this repo (sparse: `site/` only) to `/opt/spoor-site` and runs:

- `deploy/docker-compose.yml`: an `nginx:alpine` container serving `public/` read-only on the reverse proxy's
  Docker network. TLS and the domain are handled by the proxy (Nginx Proxy Manager, forwarding to `spoor-site:80`).
- `deploy/spoor-site-pull.timer`: a systemd timer that runs `git pull --ff-only` every 10 minutes. The mount is
  read-only and live, so a pull updates the site without a restart.

Set up a new host:

```sh
git clone --filter=blob:none --sparse https://github.com/asinha07/spoor /opt/spoor-site
git -C /opt/spoor-site sparse-checkout set site
cd /opt/spoor-site/site/deploy && PROXY_NETWORK=<your proxy network> docker compose up -d
cp spoor-site-pull.service spoor-site-pull.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now spoor-site-pull.timer
```
