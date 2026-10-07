package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"stable/internal/appconfig"
	"stable/internal/platform/paths"
	"stable/internal/runtime"
)

func runRemote(args []string, config appconfig.AppConfig, paths paths.Paths) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Print(remoteUsage())
		return nil
	}
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		fmt.Print(remoteUsage())
		return nil
	}
	action := args[0]
	if action != "up" && action != "down" && action != "status" && action != "pair" {
		return fmt.Errorf("unknown remote command %q; usage: stable remote up|down|status|pair", action)
	}
	flags := flag.NewFlagSet("remote "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "", "IP:port to bind (default 127.0.0.1:8765)")
	cert := flags.String("cert", "", "TLS certificate file for non-loopback listeners")
	key := flags.String("key", "", "TLS private key file for non-loopback listeners")
	if err := flags.Parse(args[1:]); err != nil {
		return fmt.Errorf("remote %s: %w", action, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if action != "up" && (*listen != "" || *cert != "" || *key != "") {
		return fmt.Errorf("remote %s does not accept --listen, --cert, or --key", action)
	}
	request := runtime.RemoteControlRequest{ListenAddr: *listen, TLSCertFile: *cert, TLSKeyFile: *key}
	switch action {
	case "up":
		if err := config.Validate(true); err != nil {
			return err
		}
		if status, err := runtime.Control(paths, "status"); err != nil || !status.Running {
			if _, err := runtime.Up(context.Background(), config, paths); err != nil {
				return fmt.Errorf("start runtime: %w", err)
			}
		}
		request.Action = runtime.RemoteControlStart
	case "down":
		if _, err := runtime.Control(paths, "status"); err != nil {
			fmt.Println("remote service is stopped (runtime is not running)")
			return nil
		}
		request.Action = runtime.RemoteControlStop
	case "status":
		if _, err := runtime.Control(paths, "status"); err != nil {
			fmt.Println("remote service is stopped")
			return nil
		}
		request.Action = runtime.RemoteControlStatus
	case "pair":
		request.Action = runtime.RemoteControlPair
	}
	result, err := runtime.RemoteControl(paths, request)
	if err != nil {
		return err
	}
	switch action {
	case "up":
		fmt.Printf("remote service listening at %s\n", result.ListenAddr)
	case "down":
		fmt.Println("remote service stopped; runtime remains running")
	case "status":
		if result.Running {
			fmt.Printf("remote service is running at %s\n", result.ListenAddr)
		} else {
			fmt.Println("remote service is stopped")
		}
	case "pair":
		if result.PairingToken == "" {
			return errors.New("runtime returned no pairing token")
		}
		fmt.Printf("Pairing token (expires %s): %s\n", result.PairExpires.Local().Format("15:04:05 MST"), result.PairingToken)
	}
	return nil
}

func remoteUsage() string {
	return `Usage:
  stable remote up [--listen IP:port] [--cert FILE --key FILE]
  stable remote down
  stable remote status
  stable remote pair

The default listener is 127.0.0.1:8765. Non-loopback listeners require TLS.
`
}
