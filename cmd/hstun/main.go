// hstun carries TCP over an ordinary HTTPS connection (WebSocket or HTTP
// request/response) with an authenticated handshake and stream multiplexing.
// `server` listens for carrier connections, `client` dials them; each can act
// as the entry (user-facing ports) or the exit (targets), so both reverse mode
// (the restricted side listens) and direct mode (the restricted side dials) are supported.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"hstun/internal/auth"
	"hstun/internal/client"
	"hstun/internal/config"
	"hstun/internal/server"
)

var version = "0.2.13"

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  hstun keygen -out FILE        write a new 32-byte secret (0600), print only its key id
  hstun check  -config FILE     validate a server or client config (-mode server|client)
  hstun server -config FILE     run the tunnel server (listening side)
  hstun client -config FILE     run the tunnel client (dialing side)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c.Run(ctx)
}
