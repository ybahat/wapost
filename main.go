package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

const usage = `usage: wapost <command>
  login     link this device by scanning a QR code (WhatsApp > Linked devices)
  channels  list channels you own or administer, with their IDs
  serve     run the HTTP endpoint (env: RELAY_TOKEN, CHANNEL_ID, LISTEN, WAPOST_DB)`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	ctx := context.Background()
	cli := newClient(ctx)

	switch os.Args[1] {
	case "login":
		login(ctx, cli)
	case "channels":
		connect(cli)
		listChannels(ctx, cli)
	case "serve":
		connect(cli)
		serve(ctx, cli)
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
	cli.Disconnect()
}

func newClient(ctx context.Context) *whatsmeow.Client {
	db := env("WAPOST_DB", "wapost.db")
	container, err := sqlstore.New(ctx, "sqlite", "file:"+db+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", waLog.Noop)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("load device: %v", err)
	}
	return whatsmeow.NewClient(device, waLog.Stdout("wa", "WARN", false))
}

func connect(cli *whatsmeow.Client) {
	if cli.Store.ID == nil {
		log.Fatal("not logged in, run: wapost login")
	}
	if err := cli.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}
	for i := 0; i < 30 && !cli.IsLoggedIn(); i++ {
		time.Sleep(time.Second)
	}
	if !cli.IsLoggedIn() {
		log.Fatal("connected but not logged in (device unlinked?), run: wapost login")
	}
}

func login(ctx context.Context, cli *whatsmeow.Client) {
	if cli.Store.ID != nil {
		fmt.Println("already logged in as", cli.Store.ID.User)
		return
	}
	qr, err := cli.GetQRChannel(ctx)
	if err != nil {
		log.Fatalf("qr: %v", err)
	}
	if err := cli.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}
	for evt := range qr {
		switch evt.Event {
		case "code":
			fmt.Println("\nScan with WhatsApp > Settings > Linked devices > Link a device:")
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
		case "success":
			fmt.Println("logged in as", cli.Store.ID.User)
			time.Sleep(3 * time.Second) // let initial sync settle
			return
		default:
			log.Fatalf("login failed: %s %v", evt.Event, evt.Error)
		}
	}
}

func listChannels(ctx context.Context, cli *whatsmeow.Client) {
	list, err := cli.GetSubscribedNewsletters(ctx)
	if err != nil {
		log.Fatalf("list channels: %v", err)
	}
	for _, n := range list {
		if n.ViewerMeta == nil {
			continue
		}
		if r := n.ViewerMeta.Role; r == types.NewsletterRoleOwner || r == types.NewsletterRoleAdmin {
			fmt.Printf("%s\t%s\t%s\n", n.ID, r, n.ThreadMeta.Name.Text)
		}
	}
}

func serve(ctx context.Context, cli *whatsmeow.Client) {
	token := os.Getenv("RELAY_TOKEN")
	if len(token) < 24 {
		log.Fatal("RELAY_TOKEN must be set (at least 24 chars)")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if !cli.IsLoggedIn() {
			http.Error(w, "whatsapp not logged in", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /post", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		text, ch, img, err := readPost(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		channel, err := parseChannel(ch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sendCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		msg := &waE2E.Message{Conversation: proto.String(text)}
		var extra whatsmeow.SendRequestExtra
		if img != nil {
			mime := http.DetectContentType(img)
			if mime != "image/jpeg" && mime != "image/png" {
				http.Error(w, "image must be jpeg or png", http.StatusBadRequest)
				return
			}
			up, err := cli.UploadNewsletter(sendCtx, img, whatsmeow.MediaImage)
			if err != nil {
				log.Printf("upload failed: %v", err)
				http.Error(w, "upload failed", http.StatusBadGateway)
				return
			}
			im := &waE2E.ImageMessage{
				Mimetype:   proto.String(mime),
				URL:        proto.String(up.URL),
				DirectPath: proto.String(up.DirectPath),
				FileSHA256: up.FileSHA256,
				FileLength: proto.Uint64(up.FileLength),
			}
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(img)); err == nil {
				im.Width, im.Height = proto.Uint32(uint32(cfg.Width)), proto.Uint32(uint32(cfg.Height))
			}
			if text != "" {
				im.Caption = proto.String(text)
			}
			msg = &waE2E.Message{ImageMessage: im}
			extra.MediaHandle = up.Handle
		}

		resp, err := cli.SendMessage(sendCtx, channel, msg, extra)
		if err != nil {
			log.Printf("send failed: %v", err)
			http.Error(w, "send failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": resp.ID})
	})

	srv := &http.Server{
		Addr:              env("LISTEN", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		<-stop
		shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// readPost accepts multipart/form-data (fields: channel, text, image), {"channel","text"} JSON,
// or a raw text body. The channel can also come from the ?channel= query parameter.
func readPost(w http.ResponseWriter, r *http.Request) (text, channel string, img []byte, err error) {
	channel = r.URL.Query().Get("channel")
	ct := r.Header.Get("Content-Type")

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		r.Body = http.MaxBytesReader(w, r.Body, maxImage+(1<<20))
		if err := r.ParseMultipartForm(maxImage); err != nil {
			return "", "", nil, fmt.Errorf("invalid form: %w", err)
		}
		text = r.FormValue("text")
		if v := r.FormValue("channel"); v != "" {
			channel = v
		}
		f, _, ferr := r.FormFile("image")
		if ferr != nil && ferr != http.ErrMissingFile {
			return "", "", nil, fmt.Errorf("read image: %w", ferr)
		}
		if f != nil {
			defer f.Close()
			if img, err = io.ReadAll(f); err != nil {
				return "", "", nil, fmt.Errorf("read image: %w", err)
			}
		}

	default:
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			return "", "", nil, fmt.Errorf("read body: %w", err)
		}
		text = string(body)
		if strings.HasPrefix(ct, "application/json") {
			var req struct {
				Channel string `json:"channel"`
				Text    string `json:"text"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				return "", "", nil, fmt.Errorf("invalid json: %w", err)
			}
			text = req.Text
			if req.Channel != "" {
				channel = req.Channel
			}
		}
	}

	if img == nil && strings.TrimSpace(text) == "" {
		return "", "", nil, fmt.Errorf("empty message")
	}
	return text, channel, img, nil
}

// parseChannel accepts "1203...@newsletter" or just the numeric part.
func parseChannel(s string) (types.JID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return types.JID{}, fmt.Errorf("missing channel")
	}
	if !strings.Contains(s, "@") {
		s += "@" + types.NewsletterServer
	}
	jid, err := types.ParseJID(s)
	if err != nil || jid.Server != types.NewsletterServer || jid.User == "" {
		return types.JID{}, fmt.Errorf("invalid channel, expected an ID like 1203...@newsletter")
	}
	return jid, nil
}

const maxImage = 16 << 20

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
