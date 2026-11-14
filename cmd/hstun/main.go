// hstun carries TCP over an ordinary HTTPS connection (WebSocket or HTTP
// request/response) with an authenticated handshake and stream multiplexing.
// `server` listens for carrier connections, `client` dials them; each can act
// as the entry (user-facing ports) or the exit (targets), so both reverse mode
// (the restricted side listens) and direct mode (the restricted side dials) are supported.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hstun/internal/auth"
	"hstun/internal/client"
	"hstun/internal/config"
	"hstun/internal/server"
)

var version = "0.5.0"

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  hstun keygen -out FILE        write a new 32-byte secret (0600), print only its key id
  hstun check  -config FILE     validate a server or client config (-mode server|client)
  hstun server -config FILE     run the tunnel server (listening side)
  hstun client -config FILE     run the tunnel client (dialing side)
  hstun probe  -config FILE     measure the path to the exit (carrier, fingerprint, sizes, soak, speed)
  hstun version
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		keygen(os.Args[2:])
	case "check":
		check(os.Args[2:])
	case "server":
		runServer(os.Args[2:])
	case "client":
		runClient(os.Args[2:])
	case "probe":
		runProbe(os.Args[2:])
	case "version":
		fmt.Println("hstun", version)
	default:
		usage()
	}
}

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "hstun:", err)
	os.Exit(1)
}

func keygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "file to create (must not exist)")
	_ = fs.Parse(args)
	if *out == "" {
		fatal(fmt.Errorf("keygen needs -out"))
	}
	k, err := auth.Generate()
	if err != nil {
		fatal(err)
	}
	if err := auth.WriteKeyFile(*out, k); err != nil {
		fatal(err)
	}
	// The key is never printed; the id only helps to tell keys apart.
	fmt.Printf("wrote %s (key id %02x)\n", *out, auth.KeyID(k))
}

func check(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := fs.String("config", "", "config file")
	mode := fs.String("mode", "", "server or client (default: guessed from the file)")
	_ = fs.Parse(args)
	if *path == "" {
		fatal(fmt.Errorf("check needs -config"))
	}
	if *mode == "" {
		if _, err := config.LoadServer(*path); err == nil {
			*mode = "server"
		} else {
			*mode = "client"
		}
	}
	switch *mode {
	case "server":
		cfg, err := config.LoadServer(*path)
		if err != nil {
			fatal(err)
		}
		if _, err := server.New(cfg, logger()); err != nil {
			fatal(err)
		}
		fmt.Println("server config ok:", cfg.Server.Listen, "role", cfg.Server.Role, "carriers", cfg.Server.Carriers)
	case "client":
		cfg, err := config.LoadClient(*path)
		if err != nil {
			fatal(err)
		}
		if _, err := client.New(cfg, logger()); err != nil {
			fatal(err)
		}
		fmt.Println("client config ok:", cfg.Client.Server, "role", cfg.Client.Role, "carrier", cfg.Client.Carrier)
	default:
		fatal(fmt.Errorf("-mode must be server or client"))
	}
}

// stopContext is cancelled by the first SIGINT or SIGTERM, which starts a
// graceful stop (streams in flight get drain_timeout to finish). It then gives
// the signals back to the runtime, so a second one ends the process at once.
func stopContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() { <-ctx.Done(); stop() }()
	return ctx
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	path := fs.String("config", "", "server config file")
	_ = fs.Parse(args)
	if *path == "" {
		fatal(fmt.Errorf("server needs -config"))
	}
	cfg, err := config.LoadServer(*path)
	if err != nil {
		fatal(err)
	}
	log := logger()
	s, err := server.New(cfg, log)
	if err != nil {
		fatal(err)
	}
	if err := s.Listen(); err != nil {
		fatal(err)
	}
	ctx := stopContext()
	if err := s.Serve(ctx); err != nil {
		fatal(err)
	}
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	path := fs.String("config", "", "client config file")
	_ = fs.Parse(args)
	if *path == "" {
		fatal(fmt.Errorf("client needs -config"))
	}
	cfg, err := config.LoadClient(*path)
	if err != nil {
		fatal(err)
	}
	c, err := client.New(cfg, logger())
	if err != nil {
		fatal(err)
	}
	if err := c.Listen(); err != nil {
		fatal(err)
	}
	c.Run(stopContext())
}

// runProbe dials one carrier session with the chosen carrier and TLS fingerprint and
// measures it: escalating echo sizes, an idle gap, a soak, bulk speed each way.
// The exit must be 0.5.0 or later with probes on; an older exit only gets the
// handshake and forward-stream check. Prints one JSON object with -json.
func runProbe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	path := fs.String("config", "", "client config file (an entry)")
	car := fs.String("carrier", "", "ws, http or chunk (default: the config's carrier; auto means ws)")
	fp := fs.String("fingerprint", "", "tls_fingerprint to use (default: the config's)")
	frag := fs.String("fragment", "", "tls_fragment to use (default: the config's)")
	soak := fs.Duration("soak", 20*time.Second, "one small echo per second for this long (0 = skip)")
	idle := fs.Duration("idle", 10*time.Second, "quiet time before one more echo (0 = skip)")
	bulk := fs.Int("bulk", 4<<20, "bytes moved each way for the speed figure (0 = skip)")
	asJSON := fs.Bool("json", false, "print one JSON object")
	_ = fs.Parse(args)
	if *path == "" {
		fatal(fmt.Errorf("probe needs -config"))
	}
	cfg, err := config.LoadClient(*path)
	if err != nil {
		fatal(err)
	}
	carrierName := *car
	if carrierName == "" {
		carrierName = cfg.Client.Carrier
	}
	if carrierName == "auto" {
		carrierName = "ws"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res := client.Probe(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), client.ProbeOptions{
		Carrier: carrierName, Fingerprint: *fp, Fragment: *frag,
		Idle: *idle, Soak: *soak, BulkDown: *bulk, BulkUp: *bulk,
	})
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(res)
	} else {
		printProbe(res)
	}
	if res.Verdict != "ok" {
		os.Exit(1)
	}
}

func printProbe(r client.ProbeResult) {
	fmt.Printf("carrier %s, fingerprint %s, fragment %s: %s (%.0f s)\n", r.Carrier, r.Fingerprint, r.Fragment, r.Verdict, r.Seconds)
	if !r.Connected {
		fmt.Println("  no session:", r.Error)
		return
	}
	fmt.Printf("  session up in %.0f ms\n", r.ConnectMs)
	if !r.ExitProbe {
		fmt.Printf("  the exit does not answer probe streams (older than 0.5.0, or disable_probe); forward stream opens: %v\n", r.ForwardOK)
		return
	}
	for _, s := range r.Steps {
		if s.OK {
			fmt.Printf("  echo %7d bytes ok   %7.0f ms\n", s.Bytes, s.Ms)
		} else {
			fmt.Printf("  echo %7d bytes FAIL %s\n", s.Bytes, s.Err)
		}
	}
	if r.IdleOK != nil {
		fmt.Printf("  after the idle gap: %v\n", *r.IdleOK)
	}
	if r.SoakSent > 0 {
		fmt.Printf("  soak: %d sent, %d lost, median %.0f ms\n", r.SoakSent, r.SoakLost, r.SoakP50Ms)
	}
	if r.DownMbit > 0 || r.UpMbit > 0 {
		fmt.Printf("  speed: down %.1f Mbit/s, up %.1f Mbit/s\n", r.DownMbit, r.UpMbit)
	}
}
