# Deploying onetrickle on a VPS

A minimal, single-user deployment: the Go binary runs as a systemd service
bound to localhost with HTTP Basic Auth, behind Caddy (or nginx) for TLS.

```
internet ──▶ Caddy/nginx (:443, TLS) ──▶ onetrickle (127.0.0.1:8080, Basic Auth)
```

Files in this directory:

| File | Goes to |
|---|---|
| `onetrickle.service` | `/etc/systemd/system/onetrickle.service` |
| `onetrickle.env.example` | copy to `/etc/onetrickle/onetrickle.env` |
| `Caddyfile` | `/etc/caddy/Caddyfile` (recommended) |
| `nginx.conf` | `/etc/nginx/sites-available/onetrickle` (alternative) |

Commands below assume Debian/Ubuntu and `sudo`. Replace `cpm.example.com` with
your domain.

## 1. Build the binary

On the VPS (needs Go), or cross-compile from your machine:

```sh
# from the repo root
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o onetrickle ./cmd/onetrickle
```

Copy it into place and make it executable:

```sh
sudo install -m 755 onetrickle /usr/local/bin/onetrickle
```

## 2. Create the service user and data directory

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin onetrickle
sudo install -d -o onetrickle -g onetrickle /var/lib/onetrickle
```

The server starts with an empty model if `/var/lib/onetrickle` has no snapshot.
To load the GolfTrickle demo instead, seed it as the service user:

```sh
sudo -u onetrickle /usr/local/bin/onetrickle seed -data /var/lib/onetrickle
```

## 3. Set the login credentials

```sh
sudo install -d /etc/onetrickle
sudo install -m 600 -o onetrickle -g onetrickle \
     deploy/onetrickle.env.example /etc/onetrickle/onetrickle.env
sudoedit /etc/onetrickle/onetrickle.env   # set ONETRICKLE_AUTH_USER / _PASS
```

Pick a long random password, e.g. `openssl rand -base64 24`.

## 4. Install and start the service

```sh
sudo install -m 644 deploy/onetrickle.service /etc/systemd/system/onetrickle.service
sudo systemctl daemon-reload
sudo systemctl enable --now onetrickle
systemctl status onetrickle           # should be active (running)
curl -u admin:yourpass localhost:8080/api/health   # {"ok":true}
```

## 5. Put a reverse proxy in front (TLS)

Make sure DNS for `cpm.example.com` points at the server and ports 80/443 are
open.

### Option A — Caddy (recommended, automatic HTTPS)

```sh
sudo apt install -y caddy
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudoedit /etc/caddy/Caddyfile          # set your domain
sudo systemctl reload caddy
```

Caddy fetches and renews the certificate automatically.

### Option B — nginx + certbot

```sh
sudo apt install -y nginx certbot python3-certbot-nginx
sudo cp deploy/nginx.conf /etc/nginx/sites-available/onetrickle
sudoedit /etc/nginx/sites-available/onetrickle   # set your domain
sudo ln -s /etc/nginx/sites-available/onetrickle /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
sudo certbot --nginx -d cpm.example.com          # adds TLS + HTTP->HTTPS redirect
```

## 6. Firewall

Only the proxy needs to be reachable; onetrickle stays on localhost.

```sh
sudo ufw allow OpenSSH
sudo ufw allow 80,443/tcp
sudo ufw enable
```

Browse to `https://cpm.example.com` — the browser will prompt for the username
and password you set in step 3.

## Updating

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o onetrickle ./cmd/onetrickle
sudo install -m 755 onetrickle /usr/local/bin/onetrickle
sudo systemctl restart onetrickle
```

The JSON snapshot in `/var/lib/onetrickle` is the only state — back it up to
keep your data.
