// Command madicloud is the MadiCloud command-line client.
//
// It talks only to the madicloudd HTTP API at MADICLOUD_API_ADDR
// (default http://127.0.0.1:8080). It never connects to PostgreSQL.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"madicloud/internal/client"
	"madicloud/internal/version"
)

const (
	envAPIAddr     = "MADICLOUD_API_ADDR"
	defaultAPIAddr = "http://127.0.0.1:8080"

	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const usage = `Usage:
  madicloud app create <name>   create an application
  madicloud app list            list applications
  madicloud app get <id>        show an application
  madicloud app delete <id>     delete an application
  madicloud help                show this help

Environment:
  MADICLOUD_API_ADDR   API address (default http://127.0.0.1:8080)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "app":
	default:
		fmt.Fprintf(stderr, "madicloud: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}

	if len(args) < 2 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	sub, rest := args[1], args[2:]
	want := map[string]int{"create": 1, "list": 0, "get": 1, "delete": 1}
	n, ok := want[sub]
	if !ok {
		fmt.Fprintf(stderr, "madicloud: unknown app command %q\n\n%s", sub, usage)
		return exitUsage
	}
	if len(rest) != n {
		fmt.Fprintf(stderr, "madicloud: app %s takes %d argument(s), got %d\n\n%s", sub, n, len(rest), usage)
		return exitUsage
	}

	addr := getenv(envAPIAddr)
	if strings.TrimSpace(addr) == "" {
		addr = defaultAPIAddr
	}
	c, err := client.New(addr, client.WithUserAgent("madicloud/"+version.Version))
	if err != nil {
		fmt.Fprintf(stderr, "madicloud: %s: %v\n", envAPIAddr, err)
		return exitUsage
	}

	switch sub {
	case "create":
		err = appCreate(ctx, c, rest[0], stdout)
	case "list":
		err = appList(ctx, c, stdout)
	case "get":
		err = appGet(ctx, c, rest[0], stdout)
	case "delete":
		err = appDelete(ctx, c, rest[0], stdout)
	}
	if err != nil {
		printError(stderr, c, err)
		return exitError
	}
	return exitOK
}

func appCreate(ctx context.Context, c *client.Client, name string, w io.Writer) error {
	app, err := c.CreateApplication(ctx, name, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Created application %s\n", app.Name)
	printApp(w, app)
	return nil
}

func appList(ctx context.Context, c *client.Client, w io.Writer) error {
	apps, err := c.ListApplications(ctx)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		fmt.Fprintln(w, "No applications.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tDESIRED STATE\tCREATED")
	for _, a := range apps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, a.ID, a.DesiredState, formatTime(a.CreatedAt))
	}
	return tw.Flush()
}

func appGet(ctx context.Context, c *client.Client, id string, w io.Writer) error {
	app, err := c.GetApplication(ctx, id)
	if err != nil {
		return err
	}
	printApp(w, app)
	return nil
}

func appDelete(ctx context.Context, c *client.Client, id string, w io.Writer) error {
	if err := c.DeleteApplication(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(w, "Deleted application %s\n", id)
	return nil
}

func printApp(w io.Writer, a client.Application) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  ID:\t%s\n", a.ID)
	fmt.Fprintf(tw, "  Name:\t%s\n", a.Name)
	fmt.Fprintf(tw, "  Desired state:\t%s\n", a.DesiredState)
	fmt.Fprintf(tw, "  Observed state:\t%s\n", "not tracked (no runtime yet)")
	fmt.Fprintf(tw, "  Created:\t%s\n", formatTime(a.CreatedAt))
	fmt.Fprintf(tw, "  Updated:\t%s\n", formatTime(a.UpdatedAt))
	_ = tw.Flush()
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func printError(w io.Writer, c *client.Client, err error) {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		fmt.Fprintf(w, "madicloud: %s\n", apiErr.Error())
		if apiErr.RequestID != "" {
			fmt.Fprintf(w, "request id: %s\n", apiErr.RequestID)
		}
		return
	}
	fmt.Fprintf(w, "madicloud: cannot reach the MadiCloud API at %s: %v\n", c.BaseURL(), err)
}
