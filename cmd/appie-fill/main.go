// Command appie-fill fills an open Albert Heijn delivery order from the
// terminal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/szwaan/appie/internal/ah"
	"github.com/szwaan/appie/internal/fill"
	"github.com/szwaan/appie/internal/tui"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "token file (default $XDG_CONFIG_HOME/appie/config.json, shared with the appie CLI)")
	flag.Parse()

	// Bubble Tea handles SIGINT and SIGTERM only while the UI runs, and a
	// closed terminal sends SIGHUP. Turning all three into cancellation ends
	// every phase, logging in included, through the deferred leaveOnExit
	// instead of killing the process with an order reopened.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	unlock, err := lockInstance()
	if err != nil {
		fmt.Fprintln(os.Stderr, "appie-fill:", err)
		return 1
	}
	defer unlock()

	client, err := ah.Open(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "appie-fill:", err)
		return 1
	}
	acct := fill.NewAccount(client)
	defer leaveOnExit(acct)

	needLogin := !client.SignedIn()
	for {
		if needLogin {
			fmt.Println("Opening ah.nl in your browser to log in…")
			if err := client.Login(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "appie-fill: login:", err)
				return 1
			}
		}
		err := tui.Run(ctx, acct)
		switch {
		case errors.Is(err, fill.ErrSignedOut):
			needLogin = true
			continue
		case ctx.Err() != nil:
			return 1
		case err != nil:
			fmt.Fprintln(os.Stderr, "appie-fill:", err)
			return 1
		}
		return 0
	}
}

// leaveOnExit is the last chance to keep the account off a reopened order.
// It runs on every exit the process controls; a run killed outright is
// caught by the next start, which lists the order as Reopened.
func leaveOnExit(acct *fill.Account) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := acct.Close(ctx)
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "appie-fill:", err)
	if errors.Is(err, fill.ErrStillReopened) {
		fmt.Fprintln(os.Stderr, "Your account stays on that order's bonus prices until it is left. Start appie-fill again to leave it.")
	}
}
