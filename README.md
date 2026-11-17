# wapost

Minimal WhatsApp client that does one thing: receives an HTTP request and posts its text to a WhatsApp channel you administer.
It is a single static Go binary built on [whatsmeow](https://github.com/tulir/whatsmeow), with no browser and no Docker. It uses about 20–40 MB of RAM.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o wapost-linux-amd64 .
```

## Deploy on a VPS (systemd)

```sh
scp wapost-linux-amd64 vps:/usr/local/bin/wapost
scp wapost.service vps:/etc/systemd/system/

# on the VPS
sudo useradd -r -d /var/lib/wapost -m wapost
cd /var/lib/wapost

# 1. Link the device: a QR code prints in the terminal. Scan it from the phone
#    in WhatsApp > Settings > Linked devices > Link a device.
sudo -u wapost wapost login

# 2. Find your channel ID
sudo -u wapost wapost channels
#   120363000000000000@newsletter   owner   My Channel

# 3. Configure
sudo tee /etc/wapost.env >/dev/null <<EOF
RELAY_TOKEN=$(openssl rand -hex 32)
LISTEN=:8080
EOF
sudo chmod 600 /etc/wapost.env

sudo systemctl enable --now wapost
```

## Use

Every request must name the target channel. Pass it as `channel` in the JSON body or form, or as the `?channel=` query parameter. Both `120363000000000000@newsletter` and the bare number `120363000000000000` work.
WhatsApp rejects posts to channels you don't own or administer.

```sh
curl -X POST https://your-vps/post \
  -H "Authorization: Bearer $RELAY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"channel":"120363000000000000@newsletter","text":"Hello channel"}'
# {"id":"3EB0..."}

# Plain text works too, with the channel in the query string
curl -X POST "https://your-vps/post?channel=120363000000000000" -H "Authorization: Bearer $RELAY_TOKEN" --data-binary 'Hello'
```

To send an image, use a multipart form. The image must be JPEG or PNG and at most 16 MB; `text` becomes the caption and is optional.

```sh
curl -X POST https://your-vps/post -H "Authorization: Bearer $RELAY_TOKEN" \
  -F "channel=120363000000000000" -F "text=Caption here" -F "image=@photo.jpg"
```

`GET /health` returns `200 ok` while WhatsApp is logged in and `503` otherwise.

## Notes

- **Put TLS in front of it.** The bearer token travels in a header, so use Caddy (`your.domain { reverse_proxy localhost:8080 }`) and set `LISTEN=127.0.0.1:8080`.
- **`wapost.db` contains the WhatsApp session keys.** Anyone who has this file can act as your linked device. Keep it private, and set `WAPOST_DB` to a path outside synced folders such as Dropbox.
- **If WhatsApp unlinks the device,** `/health` returns 503. Run `wapost login` again to fix it.
