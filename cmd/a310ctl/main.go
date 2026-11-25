// a310ctl configures an Intel Arc A310 from a YAML file and exports its telemetry.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"a310ctl/internal/client"
	"a310ctl/internal/config"
	"a310ctl/internal/controller"
	"a310ctl/internal/daemon"
)

var version = "dev"

const usage = `a310ctl - Intel Arc A310 control (configuration lives in the YAML file)

usage:
  a310ctl serve   [--config PATH]   run the daemon (default ` + config.DefaultPath + `)
  a310ctl check   [--config PATH]   validate a configuration file
  a310ctl status  [--socket PATH]   current settings and limits
  a310ctl sensors [watch] [--socket PATH]
                                    telemetry snapshot, or a live table with "watch"
  a310ctl reset   [all|power|temperature|overclock] [--socket PATH]
                                    factory reset (default all)
  a310ctl version

--socket defaults to ` + config.DefaultSocket + `; without a running daemon the
command uses the hardware directly. Commands that access the card need root.
`

// parse separates positional arguments from --name value / --name=value options.
func parse(arguments []string, allowed ...string) (positional []string, options map[string]string, err error) {
	options = map[string]string{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if !strings.HasPrefix(argument, "--") {
			positional = append(positional, argument)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
		known := false
		for _, option := range allowed {
			known = known || option == name
		}
		if !known {
			return nil, nil, fmt.Errorf("unknown option --%s", name)
		}
		if !hasValue {
			if index+1 >= len(arguments) {
				return nil, nil, fmt.Errorf("--%s needs a value", name)
			}
			index++
			value = arguments[index]
		}
		options[name] = value
	}
	return positional, options, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// run dispatches a command.
func run(arguments []string) error {
	if len(arguments) == 0 {
		fmt.Print(usage)
		return nil
	}
	command, rest := arguments[0], arguments[1:]
	switch command {
	case "serve", "check":
		positional, options, err := parse(rest, "config")
		if err != nil {
			return err
		}
		if len(positional) > 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
		path := orDefault(options["config"], config.DefaultPath)
		if command == "check" {
			if _, err := config.Load(path); err != nil {
				return err
			}
			fmt.Printf("%s: ok\n", path)
			return nil
		}
		daemon.Version = version
		return daemon.Serve(path)
	case "status", "sensors", "reset":
		positional, options, err := parse(rest, "socket")
		if err != nil {
			return err
		}
		watch, group, extra := false, "all", positional
		switch {
		case command == "sensors" && len(positional) > 0 && positional[0] == "watch":
			watch, extra = true, positional[1:]
		case command == "reset" && len(positional) > 0:
			group, extra = positional[0], positional[1:]
		}
		if len(extra) > 0 {
			return fmt.Errorf("unexpected argument %q", extra[0])
		}
		if command == "reset" {
			valid := false
			for _, name := range controller.Groups {
				valid = valid || name == group
			}
			if !valid {
				return fmt.Errorf("unknown group %q (use %s)", group, strings.Join(controller.Groups, ", "))
			}
		}
		backend, err := client.Connect(orDefault(options["socket"], config.DefaultSocket))
		if err != nil {
			return err
		}
		defer backend.Close()
		// First Ctrl+C/SIGTERM: `watch` stops at the next sample, other commands finish
		// (about a second) and the backend closes normally. A second signal kills.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(signals)
		go func() {
			select {
			case received := <-signals:
				signal.Stop(signals)
				if watch {
					interrupted.Store(int32(received.(syscall.Signal)))
					cancel()
				}
			case <-ctx.Done():
			}
		}()
		switch {
		case command == "status":
			status, err := backend.Status()
			if err != nil {
				return err
			}
			client.PrintStatus(os.Stdout, status)
		case watch:
			return client.Watch(ctx, os.Stdout, backend)
		case command == "sensors":
			sensors, err := backend.Sensors(1e9)
			if err != nil {
				return err
			}
			client.PrintSensors(os.Stdout, sensors)
		default:
			if err := backend.Reset(group); err != nil {
				return err
			}
			fmt.Printf("factory reset: %s\n", group)
		}
		return nil
	case "version", "--version", "help", "--help", "-h":
		if len(rest) > 0 {
			return fmt.Errorf("unexpected argument %q", rest[0])
		}
		if strings.HasSuffix(command, "version") {
			fmt.Println(version)
		} else {
			fmt.Print(usage)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q (see a310ctl help)", command)
}

// interrupted holds the signal that stopped `sensors watch` (0 = none).
var interrupted atomic.Int32

func main() {
	err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "a310ctl: %v\n", err)
	}
	if received := interrupted.Load(); received != 0 {
		os.Exit(128 + int(received)) // 130 for Ctrl+C, 143 for SIGTERM
	}
	if err != nil {
		os.Exit(1)
	}
}
